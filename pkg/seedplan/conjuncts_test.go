package seedplan

import (
	"reflect"
	"testing"

	"github.com/reliant-labs/forge/pkg/schemadef"
)

// The CHECK spellings below are pg_get_constraintdef's, as introspection
// returns them (check_excluded_pg_test.go applies the source SQL to postgres).

func TestExcludedLiterals_TopLevelTermsOnly(t *testing.T) {
	tbl := schemadef.Table{
		Name: "machine_pools",
		Checks: []schemadef.CheckConstraint{
			check("machine_pools_selector_present", "CHECK ((selector <> '{}'::jsonb))", "selector"),
			check("label_check", "CHECK ((label <> ''::text))", "label"),
			check("kind_check", "CHECK (((kind <> ALL (ARRAY['a'::text, 'b'::text])) AND (length(kind) > 0)))", "kind"),
			// Under an OR the exclusion is one branch, not a requirement.
			check("status_guard", "CHECK (((status <> 'DONE'::text) OR (done_at IS NOT NULL)))", "status", "done_at"),
		},
	}
	for col, want := range map[string][]string{
		"selector": {"{}"},
		"label":    {""},
		"kind":     {"a", "b"},
		"status":   nil,
	} {
		if got := ExcludedLiterals(tbl, col); !reflect.DeepEqual(got, want) {
			t.Errorf("ExcludedLiterals(%s) = %q, want %q", col, got, want)
		}
	}
}

func TestDefaultHonorsColumnChecks_ExcludedDefaultAndConjunctBound(t *testing.T) {
	selector := schemadef.Column{Name: "selector", Type: schemadef.TypeJSON, NotNull: true, Default: "'{}'::jsonb"}
	label := schemadef.Column{Name: "label", Type: schemadef.TypeString, NotNull: true, Default: "''::text"}
	floor := schemadef.Column{Name: "floor", Type: schemadef.TypeInt, NotNull: true, Default: "0"}
	ceiling := schemadef.Column{Name: "ceiling", Type: schemadef.TypeInt, NotNull: true, Default: "0"}
	tbl := schemadef.Table{
		Name:    "pools",
		Columns: []schemadef.Column{selector, label, floor, ceiling},
		Checks: []schemadef.CheckConstraint{
			check("selector_present", "CHECK ((selector <> '{}'::jsonb))", "selector"),
			check("label_check", "CHECK ((label <> ''::text))", "label"),
			check("bounds", "CHECK (((floor >= 1) AND (ceiling >= 0)))", "floor", "ceiling"),
		},
	}
	for _, tc := range []struct {
		col  schemadef.Column
		want bool
	}{
		{selector, false}, // the DEFAULT is the excluded document
		{label, false},    // the DEFAULT is the excluded string
		{floor, false},    // 0 is below the conjunct's floor of 1
		{ceiling, true},   // 0 satisfies its conjunct
	} {
		if got := defaultHonorsColumnChecks(tbl, tc.col, nil); got != tc.want {
			t.Errorf("defaultHonorsColumnChecks(%s) = %t, want %t", tc.col.Name, got, tc.want)
		}
	}
}

func TestJSONDocumentFor_AvoidsExcludedDocument(t *testing.T) {
	obj := schemadef.Column{Name: "selector", Type: schemadef.TypeJSON, NotNull: true, Default: "'{}'::jsonb"}
	arr := schemadef.Column{Name: "tags", Type: schemadef.TypeJSON, NotNull: true, Default: "'[]'::jsonb"}
	free := schemadef.Column{Name: "meta", Type: schemadef.TypeJSON, NotNull: true, Default: "'{\"a\": 1}'::jsonb"}
	tbl := schemadef.Table{
		Name:    "pools",
		Columns: []schemadef.Column{obj, arr, free},
		Checks: []schemadef.CheckConstraint{
			check("selector_present", "CHECK ((selector <> '{}'::jsonb))", "selector"),
			check("tags_present", "CHECK ((tags <> '[]'::jsonb))", "tags"),
			// Whitespace differs from the DEFAULT; jsonb compares values.
			check("meta_changed", "CHECK ((meta <> '{\"a\":1}'::jsonb))", "meta"),
		},
	}
	for _, tc := range []struct {
		col  schemadef.Column
		want string
	}{
		{obj, `{"key": "value"}`},
		{arr, `["value"]`},
		{free, `{"key": "value"}`},
	} {
		if got := JSONDocumentFor(tbl, tc.col); got != tc.want {
			t.Errorf("JSONDocumentFor(%s) = %s, want %s", tc.col.Name, got, tc.want)
		}
	}
	// No exclusion: the DEFAULT, exactly as before.
	plain := schemadef.Table{Name: "pools", Columns: []schemadef.Column{obj}}
	if got := JSONDocumentFor(plain, obj); got != "{}" {
		t.Errorf("JSONDocumentFor with no exclusion = %s, want the DEFAULT {}", got)
	}
}
