package seedplan

import (
	"reflect"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/schemadef"
)

// The CHECK spellings below are pg_get_constraintdef's, as introspection
// returns them (check_excluded_pg_test.go applies the source SQL to postgres).

func TestBuildPlan_ContradictoryConjunctBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		def  string
	}{
		{"lower_first", "CHECK (((x >= 10) AND (x <= 0) AND (y >= 0)))"},
		{"upper_first", "CHECK (((x <= 0) AND (x >= 10) AND (y >= 0)))"},
		{"strict", "CHECK (((x > 0) AND (x <= 0) AND (y >= 0)))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl := schemadef.Table{
				Name: "bounds_test",
				Columns: []schemadef.Column{
					{Name: "x", Type: schemadef.TypeInt, NotNull: true},
					{Name: "y", Type: schemadef.TypeInt, NotNull: true},
				},
				Checks: []schemadef.CheckConstraint{check("impossible_range", tc.def, "x", "y")},
			}
			p, err := BuildPlan([]schemadef.Table{tbl}, nil, Config{})
			if err == nil || p != nil {
				t.Fatalf("BuildPlan = %v, %v; want refusal before SQL generation", p, err)
			}
			for _, want := range []string{"bounds_test", "impossible_range", "contradictory bounds on x"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestBuildPlan_NullableContradictoryConjunctBounds(t *testing.T) {
	tbl := schemadef.Table{
		Name: "bounds_test",
		Columns: []schemadef.Column{
			{Name: "x", Type: schemadef.TypeInt},
			{Name: "y", Type: schemadef.TypeInt, NotNull: true},
		},
		Checks: []schemadef.CheckConstraint{
			check("nullable_range", "CHECK (((x >= 10) AND (x <= 0) AND (y >= 0)))", "x", "y"),
		},
	}
	// x = NULL, y = 0 makes the CHECK UNKNOWN, which PostgreSQL accepts.
	p, err := BuildPlan([]schemadef.Table{tbl}, nil, Config{})
	if err != nil || p == nil {
		t.Fatalf("BuildPlan = %v, %v; want a plan for nullable contradictory bounds", p, err)
	}
}

func TestSameColumnValue_ExactJSONNumbers(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		want bool
	}{
		{"large_distinct", `9007199254740992`, `9007199254740993`, false},
		{"large_equal", `9007199254740993`, `9007199254740993.0`, true},
		{"fraction_distinct", `0.12345678901234567890`, `0.12345678901234567891`, false},
		{"exponent_equal", `1e30`, `1000000000000000000000000000000.0`, true},
		{"nested_distinct", `{"n":[9007199254740992]}`, `{"n":[9007199254740993]}`, false},
		{"nested_equal", `{"n":[1,2.0]}`, `{"n":[1e0,2]}`, true},
		{"number_not_string", `1`, `"1"`, false},
		{"different_null_keys", `{"a":null}`, `{"b":null}`, false},
		{"trailing_data", `1 2`, `1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := schemadef.Column{Type: schemadef.TypeJSON}
			if got := sameColumnValue(col, tc.a, tc.b); got != tc.want {
				t.Errorf("sameColumnValue(%s, %s) = %v; want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestSameColumnValue_ExactNumericExclusions(t *testing.T) {
	for _, tc := range []struct {
		name string
		col  schemadef.Column
		a, b string
		want bool
	}{
		{"adjacent_bigints", schemadef.Column{Type: schemadef.TypeInt, DeclType: "BIGINT"}, "9007199254740992", "9007199254740993", false},
		{"bigint_max", schemadef.Column{Type: schemadef.TypeInt}, "9223372036854775807", "+9223372036854775807", true},
		{"bigint_min", schemadef.Column{Type: schemadef.TypeInt}, "-9223372036854775808", "-9223372036854775807", false},
		{"bigint_overflow", schemadef.Column{Type: schemadef.TypeInt}, "9223372036854775808", "+9223372036854775808", false},
		{"integer_leading_zero", schemadef.Column{Type: schemadef.TypeInt}, "010", "10", true},
		{"decimal_precision", schemadef.Column{Type: schemadef.TypeFloat, DeclType: "NUMERIC(30,10)"}, "9007199254740992.1", "9007199254740992.2", false},
		{"decimal_scale", schemadef.Column{Type: schemadef.TypeFloat, DeclType: "DECIMAL"}, "1.00", "1", true},
		{"decimal_exponent", schemadef.Column{Type: schemadef.TypeFloat, DeclType: "numeric"}, "9007199254740993", "9.007199254740993e15", true},
		{"decimal_invalid", schemadef.Column{Type: schemadef.TypeFloat, DeclType: "NUMERIC"}, "invalid", "1", false},
		{"float_rounding", schemadef.Column{Type: schemadef.TypeFloat, DeclType: "DOUBLE PRECISION"}, "9007199254740992", "9007199254740993", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := literalExcluded(tc.col, tc.a, []string{tc.b}); got != tc.want {
				t.Errorf("literalExcluded(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

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
