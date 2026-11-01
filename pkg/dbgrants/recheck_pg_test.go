package dbgrants

import (
	"context"
	"slices"
	"testing"

	"github.com/reliant-labs/forge/pkg/schemadef"
)

// The apply-time re-check (recheck.go), against real Postgres: a grants file
// older than the schema grants no column whose current comment does not allow
// it, and says which it withheld.

func (f *pgFixture) applyDashboard(tables []Table) []string {
	f.t.Helper()
	script, err := RenderDashboardFor(f.role, tables)
	if err != nil {
		f.t.Fatal(err)
	}
	refused, err := ApplyDashboard(context.Background(), f.db, script)
	if err != nil {
		f.t.Fatalf("apply: %v\n%s", err, script)
	}
	return refused
}

// The review's case: a declared column dropped and re-added under the same
// name, and the file NOT regenerated. The row still names it; the new
// column's comment (none: DROP COLUMN took the old one) does not allow it.
func TestDashboardRecheck_StaleFileDoesNotGrantAReAddedColumn(t *testing.T) {
	f := newPGFixture(t, convergeSchemaSQL+dashCommentsSQL)
	stale := dashTablesFor() // rendered while events.model declared forge:dashboard
	if refused := f.applyDashboard(stale); refused != nil {
		t.Fatalf("a current file refused %v", refused)
	}
	f.allowed(`SELECT id, model FROM events`, "declared columns, current file")

	f.exec(`ALTER TABLE events DROP COLUMN model`)
	f.exec(`ALTER TABLE events ADD COLUMN model TEXT NOT NULL DEFAULT 'customer@example.test'`)
	refused := f.applyDashboard(stale)
	f.denied(`SELECT model FROM events`, "a re-added column the stale file still lists")
	f.allowed(`SELECT id FROM events`, "a declared column the stale file lists")
	if want := []string{"public.events.model"}; !slices.Equal(refused, want) {
		t.Errorf("refused = %v, want %v", refused, want)
	}

	// Declared again by a migration: the same stale file grants it.
	f.exec(`COMMENT ON COLUMN events.model IS 'forge:dashboard'`)
	if refused := f.applyDashboard(stale); refused != nil {
		t.Errorf("refused = %v after the column was declared again", refused)
	}
	f.allowed(`SELECT model FROM events`, "re-declared")
}

// Every way a comment can stop allowing a listed column is a refusal: the
// table's marker gone, the column's allow gone or moved into prose, a deny
// added.
func TestDashboardRecheck_EachLostAllowIsRefused(t *testing.T) {
	for name, change := range map[string]string{
		"table marker removed":    `COMMENT ON TABLE events IS NULL`,
		"table marker in prose":   `COMMENT ON TABLE events IS 'was forge:dashboard'`,
		"column allow removed":    `COMMENT ON COLUMN events.model IS NULL`,
		"column allow in prose":   `COMMENT ON COLUMN events.model IS 'not forge:dashboard'`,
		"column now no-dashboard": `COMMENT ON COLUMN events.model IS 'forge:no-dashboard'`,
		"column now secret":       `COMMENT ON COLUMN events.model IS 'forge:dashboard forge:secret'`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newPGFixture(t, convergeSchemaSQL+dashCommentsSQL)
			f.exec(change)
			refused := f.applyDashboard(dashTablesFor())
			f.denied(`SELECT model FROM events`, name)
			if !slices.Contains(refused, "public.events.model") {
				t.Errorf("refused = %v, want it to name public.events.model", refused)
			}
		})
	}
}

// forge_ro's file is re-checked for forge:secret: a column re-added as a
// secret under a name the file lists is not granted.
func TestReadOnlyRecheck_StaleFileDoesNotGrantANewlySecretColumn(t *testing.T) {
	f := newPGFixture(t, convergeSchemaSQL)
	stale := eventsTable(Column{Name: "id"}, Column{Name: "note"})
	f.converge(stale)
	f.allowed(`SELECT note FROM events`, "a declared column")

	f.exec(`ALTER TABLE events DROP COLUMN note`)
	f.exec(`ALTER TABLE events ADD COLUMN note TEXT`)
	f.exec(`COMMENT ON COLUMN events.note IS 'forge:secret'`)
	script, err := RenderFor(f.role, stale)
	if err != nil {
		t.Fatal(err)
	}
	refused, err := Apply(context.Background(), f.db, script)
	if err != nil {
		t.Fatal(err)
	}
	f.denied(`SELECT note FROM events`, "a re-added secret column the stale file lists")
	f.allowed(`SELECT id FROM events`, "a declared column")
	if want := []string{"public.events.note"}; !slices.Equal(refused, want) {
		t.Errorf("refused = %v, want %v", refused, want)
	}
}

// A script rendered before the re-check sets nothing, so Apply must not
// report what an earlier script on the same session refused.
func TestApplyDoesNotReportAnEarlierScriptsRefusals(t *testing.T) {
	f := newPGFixture(t, convergeSchemaSQL+dashCommentsSQL)
	ctx := context.Background()
	conn, err := f.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	f.exec(`COMMENT ON COLUMN events.model IS NULL`)
	script, err := RenderDashboardFor(f.role, dashTablesFor())
	if err != nil {
		t.Fatal(err)
	}
	if refused, err := ApplyDashboard(ctx, conn, script); err != nil || len(refused) != 1 {
		t.Fatalf("first apply: refused=%v err=%v, want one refusal", refused, err)
	}
	if refused, err := ApplyDashboard(ctx, conn, `SELECT 1`); err != nil || refused != nil {
		t.Errorf("a script with no re-check reported %v (err %v), want nothing", refused, err)
	}
}

// The re-check reads the allow with the grammar `forge generate` reads it
// with. Postgres's ARE and Go's regexp must agree on every comment, or a
// current file would be refused (or a stale one trusted) by the engine that
// disagrees.
func TestMarkerListPatternAgreesWithPostgres(t *testing.T) {
	f := newPGFixture(t, `SELECT 1`)
	comments := []string{
		"", "forge:dashboard", "forge:dashboard — spend by model", "forge:dashboard\tusage",
		"forge:append-only forge:dashboard", "forge:go-type=x.Kind forge:dashboard",
		"forge:go-type=github.com/a/b;b.Kind forge:dashboard forge:immutable",
		"forge:dashboard forge:readable — a hash", "usage rollups\nforge:dashboard",
		"forge:dashboard\r\n", "  forge:dashboard  ", "first line\n  forge:no-entity forge:dashboard\nmore",
		"not forge:dashboard", "forge:readable — a hash; forge:dashboard", "forge:no-entity, forge:dashboard.",
		"forge:dashboard=yes", "forge:dashboard.", "forge:dashboard-ish", "forge:dashboards",
		"xforge:dashboard", "see docs/forge:dashboard", "forge:no-dashboard", "- forge:dashboard",
		"forge:Dashboard", "forge:x= forge:dashboard", "forge:dashboard\nnot forge:dashboard",
		"forge:secret", "forge:secret — a key", "forge:secretly", "forge:secret=x", "a forge:secret here",
		"forge:no-dashboard — an email", "forge:no-dashboard-ish", "forge:readable — not for forge:no-dashboards",
	}
	ctx := context.Background()
	for _, c := range comments {
		var allow, secret, dashDeny bool
		if err := f.db.QueryRowContext(ctx, `SELECT $1 ~ $2, $1 ~ $3, $1 ~ $4`,
			c, "(?n)"+dashboardAllowPattern, secretDenyPattern, dashboardDenyPattern).Scan(&allow, &secret, &dashDeny); err != nil {
			t.Fatalf("%q: %v", c, err)
		}
		col := schemadef.Column{Comment: c}
		if want := (schemadef.Table{Comment: c}).Dashboard(); allow != want {
			t.Errorf("allow %q: postgres %v, schemadef %v", c, allow, want)
		}
		if want := col.DeclaresDashboard(); allow != want {
			t.Errorf("column allow %q: postgres %v, schemadef %v", c, allow, want)
		}
		if want := col.DeclaresSecret(); secret != want {
			t.Errorf("secret %q: postgres %v, schemadef %v", c, secret, want)
		}
		// The dashboard deny may be broader than Go's (it can only withhold),
		// but never narrower.
		if want := col.DeclaresSecret() || col.DeclaresNoDashboard(); want && !dashDeny {
			t.Errorf("deny %q: schemadef withholds it and postgres does not", c)
		}
	}
}
