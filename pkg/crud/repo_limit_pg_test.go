package crud

import (
	"context"
	"testing"

	"github.com/reliant-labs/forge/pkg/orm"
)

// TestRepoList_DefaultLimit proves the defense-in-depth cap: a direct repo
// caller that supplies no limit is bounded by defaultListLimit, while a
// caller that passes an explicit orm.WithLimit overrides it (so the RPC path,
// which always supplies one, is unaffected). Boots embedded postgres; skipped
// under -short.
func TestRepoList_DefaultLimit(t *testing.T) {
	db := newRepoTestDB(t)
	ctx := context.Background()
	createWidgetsTable(t, db)

	// Lower the cap so we can prove it without seeding a huge table.
	orig := defaultListLimit
	defaultListLimit = 3
	t.Cleanup(func() { defaultListLimit = orig })

	repo := NewRepo[widget]()
	for i := 0; i < 5; i++ {
		if err := repo.Create(ctx, db, &widget{Name: "w"}); err != nil {
			t.Fatalf("seed Create: %v", err)
		}
	}

	// No explicit limit → capped at defaultListLimit (3), NOT all 5.
	got, err := repo.List(ctx, db)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("limit-less List should be capped at %d, got %d (unbounded scan)", defaultListLimit, len(got))
	}

	// Explicit WithLimit overrides the cap upward (proves RPC path unchanged).
	got, err = repo.List(ctx, db, orm.WithLimit(5))
	if err != nil {
		t.Fatalf("List with limit: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("explicit WithLimit(5) should override the cap, got %d", len(got))
	}

	// Explicit WithLimit smaller than the cap also wins.
	got, err = repo.List(ctx, db, orm.WithLimit(2))
	if err != nil {
		t.Fatalf("List with small limit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("explicit WithLimit(2) should win, got %d", len(got))
	}
}
