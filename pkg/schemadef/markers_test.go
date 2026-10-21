package schemadef

import (
	"reflect"
	"strings"
	"testing"
)

func softDeleteTable(deletedAtComment string) Table {
	return Table{
		Name:   "worktrees",
		PKCols: []string{"id"},
		Columns: []Column{
			{Name: "id", Type: TypeString, IsPK: true, NotNull: true},
			{Name: "branch", Type: TypeString, NotNull: true},
			{Name: "deleted_at", Type: TypeTime, Comment: deletedAtComment},
		},
	}
}

// The default is unchanged: a time-typed deleted_at is soft delete.
func TestDetectConventions_DeletedAtIsSoftDeleteByDefault(t *testing.T) {
	if !DetectConventions(softDeleteTable("")).SoftDelete {
		t.Fatal("a time-typed deleted_at with no declaration must stay soft delete")
	}
	if !DetectConventions(softDeleteTable("when the row was tombstoned")).SoftDelete {
		t.Fatal("prose in the comment is not a declaration")
	}
}

// Declared, deleted_at is an ordinary column. Fails on the old code, which
// read soft delete off the column's name and type alone.
func TestDetectConventions_NoSoftDeleteDeclaration(t *testing.T) {
	for _, comment := range []string{
		"forge:no-soft-delete",
		"forge:no-soft-delete — the archive stamp; archived worktrees stay readable",
		"archive stamp. forge:no-soft-delete",
	} {
		if DetectConventions(softDeleteTable(comment)).SoftDelete {
			t.Errorf("deleted_at commented %q is still soft delete", comment)
		}
	}
	// A longer marker that merely begins with this one is not it.
	if !DetectConventions(softDeleteTable("forge:no-soft-delete-ish")).SoftDelete {
		t.Error("forge:no-soft-delete-ish must not read as forge:no-soft-delete")
	}
}

func TestMarkerTokens(t *testing.T) {
	for comment, want := range map[string][]string{
		"":                                  nil,
		"plain prose":                       nil,
		"forge: is the generator":           nil,
		"forge:fill=ulid":                   {"forge:fill"},
		"forge:immutable forge:owner":       {"forge:immutable", "forge:owner"},
		"forge:ref derived-from=job_id":     {"forge:ref"},
		"forge:go-type=map[string]any":      {"forge:go-type"},
		"see forge:Immutable":               {"forge:Immutable"},
		"https://x/forge:y and xforge:z":    nil,
		"forge:entity key=id — the row":     {"forge:entity"},
		"forge:no-soft-delete — archive at": {"forge:no-soft-delete"},
	} {
		if got := MarkerTokens(comment); !reflect.DeepEqual(got, want) {
			t.Errorf("MarkerTokens(%q) = %q, want %q", comment, got, want)
		}
	}
}

func TestCommentMarkerProblems(t *testing.T) {
	cases := []struct {
		site    MarkerSite
		column  string
		comment string
		want    string // substring of the one problem; "" for none
	}{
		{SiteColumn, "deleted_at", "forge:no-soft-delete", ""},
		{SiteColumn, "total", "forge:immutable forge:fill=handler", ""},
		{SiteColumn, "kind", "forge:go-type=github.com/a/b;b.Kind", ""},
		{SiteConstraint, "", "forge:ref authoritative", ""},
		{SiteTable, "", "forge:append-only — a ledger", ""},
		{SiteTable, "", "forge:entity-name=Widget", ""},
		{SiteView, "", "forge:entity key=id", ""},
		{SiteColumn, "notes", "free-text notes, nothing special", ""},

		{SiteColumn, "total", "forge:immutible", "did you mean forge:immutable?"},
		{SiteColumn, "deleted_at", "forge:no-softdelete", "did you mean forge:no-soft-delete?"},
		{SiteColumn, "archived_at", "forge:no-soft-delete", "only applies to the deleted_at column"},
		{SiteColumn, "id", "forge:append-only", "is a table marker"},
		{SiteTable, "", "forge:immutable", "is a column marker"},
		{SiteColumn, "patient_id", "forge:ref authoritative", "is a constraint marker"},
		{SiteTable, "", "forge:entity", "is a view marker"},
		{SiteConstraint, "", "forge:refactor later", "not a marker forge reads"},
	}
	for _, c := range cases {
		got := CommentMarkerProblems(c.site, c.column, c.comment)
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("%s %q: unexpected problems %+v", c.site, c.comment, got)
			}
			continue
		}
		if len(got) != 1 || !strings.Contains(got[0].Problem, c.want) {
			t.Errorf("%s %q: got %+v, want one problem containing %q", c.site, c.comment, got, c.want)
		}
	}
}

func TestCheckMarkers_NamesEveryProblemWithItsObject(t *testing.T) {
	tables := []Table{
		softDeleteTable("forge:no-soft-delete"),
		{
			Name:    "invoices",
			Comment: "forge:append-only forge:appendonly",
			Columns: []Column{{Name: "total", Comment: "forge:immutible"}},
			ForeignKeys: []ForeignKey{{
				Name: "invoices_customer_id_fkey", Column: "customer_id", Comment: "forge:ref authoritative",
			}},
		},
		{Name: "v", IsView: true, Comment: "forge:no-entity"},
	}
	err := CheckMarkers(tables)
	if err == nil {
		t.Fatal("CheckMarkers accepted markers that do nothing")
	}
	for _, want := range []string{
		"table invoices: forge:appendonly is not a marker",
		"column invoices.total: forge:immutible",
		"view v: forge:no-entity is a table marker",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "worktrees") || strings.Contains(err.Error(), "constraint") {
		t.Errorf("valid declarations reported:\n%v", err)
	}
	if err := CheckMarkers([]Table{softDeleteTable("forge:no-soft-delete")}); err != nil {
		t.Errorf("a valid schema is rejected: %v", err)
	}
}

// forge:closed-set is a constraint marker read on a CHECK: the generate that
// reads the catalog accepts it there, rejects it on a foreign key, rejects
// forge:ref on a CHECK, and rejects a misspelling with a suggestion.
func TestCheckMarkers_ClosedSetOnCheckConstraints(t *testing.T) {
	table := func(checkComment, fkComment string) []Table {
		return []Table{{
			Name:        "access_tokens",
			Checks:      []CheckConstraint{{Name: "access_tokens_scopes", Comment: checkComment}},
			ForeignKeys: []ForeignKey{{Name: "access_tokens_org_fkey", Column: "org_id", Comment: fkComment}},
		}}
	}
	if err := CheckMarkers(table("forge:closed-set accesstoken.AllScopes", "forge:ref authoritative")); err != nil {
		t.Fatalf("closed-set on a CHECK and ref on an FK are both read: %v", err)
	}
	for name, tc := range map[string]struct{ check, fk, want string }{
		"closed-set on a foreign key": {"", "forge:closed-set accesstoken.AllScopes", "constraint access_tokens_org_fkey on access_tokens: forge:closed-set is read on a CHECK constraint"},
		"ref on a check":              {"forge:ref authoritative", "", "constraint access_tokens_scopes on access_tokens: forge:ref is read on a FOREIGN KEY constraint"},
		"misspelled":                  {"forge:closed-sets accesstoken.AllScopes", "", "did you mean forge:closed-set?"},
	} {
		t.Run(name, func(t *testing.T) {
			err := CheckMarkers(table(tc.check, tc.fk))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
