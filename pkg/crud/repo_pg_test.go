package crud

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/reliant-labs/forge/pkg/orm"
	"github.com/reliant-labs/forge/pkg/pgtest"
)

// These tests exercise the generic Repo against a REAL embedded postgres —
// the same correctness oracle as the e2e CRUD-lifecycle gate, but at the
// library level so a regression localizes here instead of in a scaffolded
// project. They boot postgres, so they're skipped under -short (the inner
// loop), matching the epic's velocity rule.

func newRepoTestDB(t *testing.T) orm.Context {
	t.Helper()
	if testing.Short() {
		t.Skip("repo pg tests boot embedded postgres; skipped under -short")
	}
	sqldb, cleanup, err := pgtest.New()
	if err != nil {
		t.Fatalf("pgtest.New: %v", err)
	}
	t.Cleanup(cleanup)
	client, err := orm.NewClientWithDB(sqldb, "postgres")
	if err != nil {
		t.Fatalf("orm.NewClientWithDB: %v", err)
	}
	return client
}

// widget is a tenant-scoped, soft-deleting, timestamped entity with an
// array column and a server-allocated PK — it touches every Repo code path
// at once (the lifecycle gate's "maximum coverage in one entity" stance).
type widget struct {
	bun.BaseModel `bun:"table:widgets,alias:widgets"`

	ID        int64      `bun:"id,pk,autoincrement"`
	TenantID  string     `bun:"tenant_id,notnull"`
	Name      string     `bun:"name,notnull"`
	Tags      []string   `bun:"tags,array,notnull"`
	CreatedAt time.Time  `bun:"created_at,notnull"`
	UpdatedAt time.Time  `bun:"updated_at,notnull"`
	DeletedAt *time.Time `bun:"deleted_at,soft_delete,nullzero"`
}

func createWidgetsTable(t *testing.T, db orm.Context) {
	t.Helper()
	_, err := db.Bun().NewCreateTable().Model((*widget)(nil)).
		IfNotExists().Exec(context.Background())
	if err != nil {
		t.Fatalf("create widgets table: %v", err)
	}
}

func TestRepoLifecycle_WidgetFullSurface(t *testing.T) {
	db := newRepoTestDB(t)
	ctx := context.Background()
	createWidgetsTable(t, db)

	repo := NewRepo[widget](Spec{TenantColumn: "tenant_id", Timestamps: true})
	const tenant = "tenant-A"

	// ── Create: server PK populated, timestamps stamped, nil array → {} ──
	w := &widget{Name: "alpha"} // Tags nil on purpose
	if err := repo.Create(ctx, db, w, tenant); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if w.ID == 0 {
		t.Error("Create did not read back the server-allocated PK")
	}
	if w.TenantID != tenant {
		t.Errorf("Create did not stamp tenant: got %q", w.TenantID)
	}
	if w.CreatedAt.IsZero() || w.UpdatedAt.IsZero() {
		t.Error("Create did not stamp managed timestamps")
	}

	// Read back: array bound as {} (non-NULL), tenant scope returns the row.
	got, err := repo.Get(ctx, db, w.ID, tenant)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Tags == nil {
		t.Error("nil array was not normalized to a non-nil empty slice on insert")
	}

	// Tenant isolation: a different tenant cannot read the row.
	if _, err := repo.Get(ctx, db, w.ID, "tenant-B"); !errors.Is(err, orm.ErrNoRows) {
		t.Errorf("cross-tenant Get should be ErrNoRows, got %v", err)
	}

	// ── Update (full): updated_at advances, array round-trips ──
	firstUpdatedAt := got.UpdatedAt
	time.Sleep(2 * time.Millisecond)
	got.Name = "alpha-2"
	got.Tags = []string{"x", "y"}
	if err := repo.Update(ctx, db, got, tenant); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reread, _ := repo.Get(ctx, db, w.ID, tenant)
	if reread.Name != "alpha-2" {
		t.Errorf("Update did not persist name: %q", reread.Name)
	}
	if len(reread.Tags) != 2 {
		t.Errorf("Update did not persist array: %v", reread.Tags)
	}
	if !reread.UpdatedAt.After(firstUpdatedAt) {
		t.Errorf("Update did not advance updated_at: %v !> %v", reread.UpdatedAt, firstUpdatedAt)
	}

	// ── UpdateMasked: only the named column changes (non-clobber) ──
	masked := &widget{ID: w.ID, Name: "SHOULD-NOT-WIN", Tags: []string{"only-tags"}}
	if err := repo.UpdateMasked(ctx, db, masked, []string{"tags"}, tenant); err != nil {
		t.Fatalf("UpdateMasked: %v", err)
	}
	afterMask, _ := repo.Get(ctx, db, w.ID, tenant)
	if afterMask.Name != "alpha-2" {
		t.Errorf("masked update clobbered an unmasked column: name=%q want alpha-2", afterMask.Name)
	}
	if len(afterMask.Tags) != 1 || afterMask.Tags[0] != "only-tags" {
		t.Errorf("masked update did not write the masked column: %v", afterMask.Tags)
	}

	// ── UpdateMasked: unknown / immutable path → UnknownFieldError ──
	var unknown *orm.UnknownFieldError
	err = repo.UpdateMasked(ctx, db, masked, []string{"id"}, tenant)
	if !errors.As(err, &unknown) || unknown.Field != "id" {
		t.Errorf("masked update of PK should be UnknownFieldError{id}, got %v", err)
	}

	// ── soft delete: row survives with deleted_at set, vanishes from reads ──
	if err := repo.Delete(ctx, db, w.ID, tenant); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, db, w.ID, tenant); !errors.Is(err, orm.ErrNoRows) {
		t.Errorf("soft-deleted row should be invisible to Get, got %v", err)
	}
	live, _ := repo.List(ctx, db, tenant)
	if len(live) != 0 {
		t.Errorf("soft-deleted row should be invisible to List, got %d rows", len(live))
	}
	all, _ := repo.ListAll(ctx, db, tenant)
	if len(all) != 1 {
		t.Errorf("ListAll should include the tombstone, got %d rows", len(all))
	}
	if all[0].DeletedAt == nil {
		t.Error("soft delete did not stamp deleted_at (decorative soft delete)")
	}

	// ── update-guard: an UPDATE must not resurrect/mutate a tombstone ──
	all[0].Name = "zombie"
	if err := repo.Update(ctx, db, all[0], tenant); err != nil {
		t.Fatalf("Update on tombstone: %v", err)
	}
	stillTomb, _ := repo.ListAll(ctx, db, tenant)
	if stillTomb[0].Name == "zombie" {
		t.Error("UPDATE mutated a soft-deleted row — the deleted_at IS NULL guard is missing")
	}
}
