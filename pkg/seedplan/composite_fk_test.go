package seedplan

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/pkg/pgtest"
	"github.com/reliant-labs/forge/pkg/schemadef"
)

// The multi-tenant integrity pattern: every child carries tenant_id, and a
// composite foreign key makes "a child references only a parent in its own
// tenant" a database fact — beside the single-column references the tables
// were born with.
const compositeFKMigration = `
CREATE TABLE tenants (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE TABLE strategies (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    title TEXT NOT NULL,
    UNIQUE (tenant_id, id)
);

CREATE TABLE channels (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    title TEXT NOT NULL,
    UNIQUE (tenant_id, id)
);

CREATE TABLE hypotheses (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    strategy_id TEXT NOT NULL REFERENCES strategies(id),
    channel_id TEXT REFERENCES channels(id),
    claim TEXT NOT NULL,
    FOREIGN KEY (tenant_id, strategy_id) REFERENCES strategies (tenant_id, id),
    FOREIGN KEY (tenant_id, channel_id) REFERENCES channels (tenant_id, id)
);
`

// compositeFKSchema is compositeFKMigration as introspection reports it: the
// composites one entry per column, each carrying the whole key.
func compositeFKSchema() []schemadef.Table {
	tenants := schemadef.Table{
		Name:    "tenants",
		PKCols:  []string{"id"},
		Columns: []schemadef.Column{col("id", schemadef.TypeString, true, true), col("name", schemadef.TypeString, true, false)},
	}
	parent := func(name string) schemadef.Table {
		return schemadef.Table{
			Name:   name,
			PKCols: []string{"id"},
			Columns: []schemadef.Column{
				col("id", schemadef.TypeString, true, true),
				col("tenant_id", schemadef.TypeString, true, false),
				col("title", schemadef.TypeString, true, false),
			},
			ForeignKeys: []schemadef.ForeignKey{
				{Column: "tenant_id", RefTable: "tenants", RefColumn: "id", Name: name + "_tenant_id_fkey"},
			},
		}
	}
	composite := func(name, refTable, col string) []schemadef.ForeignKey {
		keys, refs := []string{"tenant_id", col}, []string{"tenant_id", "id"}
		return []schemadef.ForeignKey{
			{Column: "tenant_id", RefTable: refTable, RefColumn: "tenant_id", Name: name, KeyColumns: keys, RefKeyColumns: refs},
			{Column: col, RefTable: refTable, RefColumn: "id", Name: name, KeyColumns: keys, RefKeyColumns: refs},
		}
	}
	hypotheses := schemadef.Table{
		Name:   "hypotheses",
		PKCols: []string{"id"},
		Columns: []schemadef.Column{
			col("id", schemadef.TypeString, true, true),
			col("tenant_id", schemadef.TypeString, true, false),
			col("strategy_id", schemadef.TypeString, true, false),
			col("channel_id", schemadef.TypeString, false, false),
			col("claim", schemadef.TypeString, true, false),
		},
		ForeignKeys: append(append([]schemadef.ForeignKey{
			{Column: "tenant_id", RefTable: "tenants", RefColumn: "id", Name: "hypotheses_tenant_id_fkey"},
			{Column: "strategy_id", RefTable: "strategies", RefColumn: "id", Name: "hypotheses_strategy_id_fkey"},
			{Column: "channel_id", RefTable: "channels", RefColumn: "id", Name: "hypotheses_channel_id_fkey"},
		}, composite("hypotheses_tenant_id_strategy_id_fkey", "strategies", "strategy_id")...),
			composite("hypotheses_tenant_id_channel_id_fkey", "channels", "channel_id")...),
	}
	return []schemadef.Table{hypotheses, parent("strategies"), parent("channels"), tenants}
}

// TestCompositeFK_EveryColumnFromOneParentRow is the shape the composite
// exists for: on every row, tenant_id is the tenant of the very strategy
// strategy_id names. Recomputed through SeedValue, the values Render emits.
func TestCompositeFK_EveryColumnFromOneParentRow(t *testing.T) {
	p := buildOrFail(t, compositeFKSchema(), Config{Rows: 20, Salt: 1})
	if err := p.Validate(); err != nil {
		t.Fatalf("a composite foreign key is the schema stating the invariant; nothing to refuse: %v", err)
	}
	strategyTenant := map[string]string{}
	for r := 0; r < p.rowsOf["strategies"]; r++ {
		id, _ := p.SeedValue("strategies", "id", r)
		tenant, _ := p.SeedValue("strategies", "tenant_id", r)
		strategyTenant[id] = tenant
	}
	channelTenant := map[string]string{}
	for r := 0; r < p.rowsOf["channels"]; r++ {
		id, _ := p.SeedValue("channels", "id", r)
		tenant, _ := p.SeedValue("channels", "tenant_id", r)
		channelTenant[id] = tenant
	}
	channels := 0
	for r := 0; r < p.rowsOf["hypotheses"]; r++ {
		tenant, _ := p.SeedValue("hypotheses", "tenant_id", r)
		strategy, _ := p.SeedValue("hypotheses", "strategy_id", r)
		owner, ok := strategyTenant[strategy]
		if !ok {
			t.Fatalf("row %d: strategy_id %q is not a seeded strategy — the single-column reference beside the composite is violated", r, strategy)
		}
		if owner != tenant {
			t.Errorf("row %d: tenant_id %q, but strategy %q belongs to tenant %q", r, tenant, strategy, owner)
		}
		if p.SeedIsNull("hypotheses", "channel_id", r) {
			continue
		}
		channel, _ := p.SeedValue("hypotheses", "channel_id", r)
		channels++
		if got, ok := channelTenant[channel]; !ok || got != tenant {
			t.Errorf("row %d: channel %q belongs to tenant %q, the row to %q — the second composite must agree on the column the first placed", r, channel, got, tenant)
		}
	}
	if channels == 0 {
		t.Fatal("no row references a channel — the shared-column case went untested")
	}
}

// TestCompositeFK_EndToEndOnRealPostgres lets postgres judge: introspect the
// applied migration, seed it, and every constraint — the composites and the
// single-column references beside them — holds.
func TestCompositeFK_EndToEndOnRealPostgres(t *testing.T) {
	requirePG(t)
	ctx := context.Background()
	db, cleanup, err := pgtest.New()
	if err != nil {
		t.Fatalf("pgtest.New: %v", err)
	}
	t.Cleanup(cleanup)
	if _, err := db.Exec(compositeFKMigration); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "00001_init.up.sql"), []byte(compositeFKMigration), 0o644); err != nil {
		t.Fatalf("write migration: %v", err)
	}

	plan, err := BuildLivePlan(ctx, db, dir, "", Config{Rows: 20, Salt: 1})
	if err != nil {
		t.Fatalf("BuildLivePlan: %v", err)
	}
	if _, err := Apply(ctx, db, plan); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := countRows(t, ctx, db, "hypotheses"); n == 0 {
		t.Fatal("no hypotheses seeded — the test proves nothing")
	}
	assertNoCrossTenant(t, ctx, db)
}

func assertNoCrossTenant(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var crossed int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM hypotheses h
		JOIN strategies s ON s.id = h.strategy_id
		WHERE s.tenant_id <> h.tenant_id`).Scan(&crossed); err != nil {
		t.Fatalf("join query: %v", err)
	}
	if crossed != 0 {
		t.Errorf("%d hypotheses name a strategy of another tenant", crossed)
	}
}
