package crud

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/orm"
)

// forge#2266: one statement for every matching live row, its count returned,
// updated_at stamped, a soft-deleted row left alone, the caller's template
// unchanged — and never an accidental write of the whole table.
func TestRepoUpdateWhere(t *testing.T) {
	db := newRepoTestDB(t)
	ctx := context.Background()
	createWidgetsTable(t, db)
	repo := NewRepo[widget]()

	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	var ids []int64
	for _, name := range []string{"open", "open", "open", "closed"} {
		w := &widget{Name: name, Tags: []string{}, CreatedAt: old, UpdatedAt: old}
		if err := repo.Create(ctx, db, w); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, w.ID)
	}
	if err := repo.Delete(ctx, db, ids[2]); err != nil { // soft delete one open row
		t.Fatal(err)
	}

	tmpl := &widget{Name: "archived"}
	n, err := repo.UpdateWhere(ctx, db, tmpl, []string{"name"}, orm.WhereEq("name", "open"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rows changed = %d, want the 2 live open rows", n)
	}
	if !tmpl.UpdatedAt.IsZero() {
		t.Error("UpdateWhere stamped the caller's template")
	}
	for i, want := range []string{"archived", "archived"} {
		got, err := repo.Get(ctx, db, ids[i])
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != want || !got.UpdatedAt.After(old) {
			t.Errorf("row %d = %q updated %v; want %q, re-stamped", ids[i], got.Name, got.UpdatedAt, want)
		}
	}
	if got, _ := repo.Get(ctx, db, ids[3]); got == nil || got.Name != "closed" {
		t.Errorf("a row the filter does not match was written: %+v", got)
	}
	all, err := repo.ListAll(ctx, db, orm.WhereEq("id", ids[2]))
	if err != nil || len(all) != 1 || all[0].Name != "open" {
		t.Errorf("the soft-deleted row was written: %+v, %v", all, err)
	}

	// A bounded batch: List's options, ordering and limit included.
	n, err = repo.UpdateWhere(ctx, db, &widget{Name: "batched"}, []string{"name"}, orm.WhereEq("name", "archived"), orm.WithOrderBy("id", orm.Asc), orm.WithLimit(1))
	if err != nil || n != 1 {
		t.Fatalf("limited batch: %d, %v; want 1", n, err)
	}

	if _, err := repo.UpdateWhere(ctx, db, tmpl, []string{"name"}); err == nil {
		t.Error("an unfiltered UpdateWhere wrote the whole table")
	}
	var unknown *orm.UnknownFieldError
	if _, err := repo.UpdateWhere(ctx, db, tmpl, []string{"nope"}, orm.WhereEq("name", "x")); !errors.As(err, &unknown) {
		t.Errorf("unknown field: %v, want *orm.UnknownFieldError", err)
	}
	if n, err := repo.UpdateWhere(ctx, db, tmpl, []string{"name"}, orm.WhereEq("name", "nothing-matches")); err != nil || n != 0 {
		t.Errorf("no match: %d, %v; want 0, nil", n, err)
	}
}
