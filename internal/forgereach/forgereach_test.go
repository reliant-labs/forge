package forgereach

import "testing"

func TestIsGenerated(t *testing.T) {
	cases := map[string]bool{
		"internal/templates/project/Taskfile.yml.tmpl":        true,
		"internal/generator/orm.go":                           true,
		"internal/codegen/writers.go":                         true,
		"internal/cli/scaffold/scaffold.go":                   true,
		"internal/cli/generate_pipeline.go":                   true,
		"internal/templates/project/skills/forge/db/SKILL.md": false,
		"internal/generator/orm_test.go":                      false,
		"internal/generator/testdata/golden/x.go":             false,
		"internal/cli/lint/lint.go":                           false,
		"internal/cli/ci.go":                                  false,
		"docs/concepts.md":                                    false,
		"pkg/orm/orm.go":                                      false,
	}
	for f, want := range cases {
		if got := isGenerated(f); got != want {
			t.Errorf("isGenerated(%q) = %v, want %v", f, got, want)
		}
	}
}

func TestReachSummary(t *testing.T) {
	if got := (Reach{}).Summary(); got != "nothing a consumer sees" {
		t.Errorf("empty summary = %q", got)
	}
	r := Reach{All: true, Packages: []string{"a"}, Generated: []string{"b"}}
	if !r.API() || !r.Any() {
		t.Error("go.mod change and package change must reach the API")
	}
	if (Reach{Generated: []string{"b"}}).API() {
		t.Error("generator-only change does not change the Go API")
	}
	if !(Reach{Generated: []string{"b"}}).Any() {
		t.Error("generator-only change still reaches consumers")
	}
}
