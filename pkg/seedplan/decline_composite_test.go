package seedplan

import (
	"testing"

	"github.com/reliant-labs/forge/pkg/schemadef"
)

// A declined composite keeps only the member IT set NULL as a single-column
// foreign key, never a sibling member that is NULL for another reason: that
// sibling would otherwise become a scalar reference to the composite's parent.
func TestDeclineCompositeFKsKeepsOnlyTheSelectedMember(t *testing.T) {
	member := func(col, ref string) schemadef.ForeignKey {
		return schemadef.ForeignKey{
			Name: "items_ab_fkey", Column: col, RefTable: "parents", RefColumn: ref,
			KeyColumns: []string{"a", "b"}, RefKeyColumns: []string{"x", "y"},
		}
	}
	items := schemadef.Table{Name: "items", ForeignKeys: []schemadef.ForeignKey{
		member("a", "x"),
		member("b", "y"),
		// c is a scalar reference beside a second declined composite on it.
		{Name: "items_c_fkey", Column: "c", RefTable: "others", RefColumn: "id"},
		{Name: "items_cd_fkey", Column: "c", RefTable: "parents", RefColumn: "x", KeyColumns: []string{"c", "d"}, RefKeyColumns: []string{"x", "y"}},
		{Name: "items_cd_fkey", Column: "d", RefTable: "parents", RefColumn: "y", KeyColumns: []string{"c", "d"}, RefKeyColumns: []string{"x", "y"}},
	}}
	byName := map[string]schemadef.Table{"items": items}
	// b is force-null too (for some other reason), but items_ab_fkey chose a.
	declined := map[string]map[string]string{"items": {"items_ab_fkey": "a", "items_cd_fkey": "c"}}

	out := declineCompositeFKs([]schemadef.Table{items}, byName, declined)

	got := map[string]schemadef.ForeignKey{}
	for _, fk := range out[0].ForeignKeys {
		if len(fk.KeyColumns) != 0 {
			t.Errorf("%s (%s) still carries a composite key %v", fk.Name, fk.Column, fk.KeyColumns)
		}
		if _, dup := got[fk.Column]; dup {
			t.Errorf("column %s has two foreign-key entries", fk.Column)
		}
		got[fk.Column] = fk
	}
	if fk, ok := got["a"]; !ok || fk.RefTable != "parents" {
		t.Errorf("selected member a: got %+v, want a scalar entry to parents", fk)
	}
	if _, ok := got["b"]; ok {
		t.Errorf("unselected member b kept a foreign-key entry")
	}
	if fk := got["c"]; fk.Name != "items_c_fkey" {
		t.Errorf("c: got %+v, want its own single-column reference only", fk)
	}
	if _, ok := got["d"]; ok {
		t.Errorf("unselected member d kept a foreign-key entry")
	}
}
