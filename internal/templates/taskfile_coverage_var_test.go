package templates

import (
	"strings"
	"testing"
	"text/template"

	sprig "github.com/go-task/slim-sprig/v3"
	"gopkg.in/yaml.v3"
)

// The lanes test `eq .COVERAGE "true"`, so the COVERAGE var must evaluate to
// exactly "true" or "false" for every spelling forge.yaml allows: the bool,
// the map form (enabled defaulting to true) and the legacy ci.test.coverage.
// The var is a Task template, evaluated here the way Task does: text/template
// with slim-sprig and Task's fromYaml.
func TestTaskfileCoverageVarIsCanonical(t *testing.T) {
	cases := []struct {
		forgeYAML string
		want      string
	}{
		{"tests:\n  coverage: true\n", "true"},
		{"tests:\n  coverage: false\n", "false"},
		{"tests:\n  coverage:\n    enabled: true\n", "true"},
		{"tests:\n  coverage:\n    enabled: false\n", "false"},
		{"tests:\n  coverage:\n    enabled: \"false\"\n", "false"},
		{"tests:\n  coverage:\n    enabled:\n", "false"},        // null: never "<no value>"
		{"tests:\n  coverage:\n    packages: module\n", "true"}, // enabled defaults to true
		{"ci:\n  test:\n    coverage: true\n", "true"},          // legacy, when tests does not say
		{"ci:\n  test:\n    coverage: false\n", "false"},
		{"name: demo\n", "false"},
	}
	funcs := sprig.TxtFuncMap()
	funcs["fromYaml"] = func(s string) map[string]any {
		m := map[string]any{}
		_ = yaml.Unmarshal([]byte(s), &m)
		return m
	}
	for _, tmpl := range taskfileTemplatesByKind {
		var doc struct {
			Vars map[string]any `yaml:"vars"`
		}
		if err := yaml.Unmarshal(renderProject(t, tmpl, projectData()), &doc); err != nil {
			t.Fatalf("%s: %v", tmpl, err)
		}
		expr, ok := doc.Vars["COVERAGE"].(string)
		if !ok {
			t.Fatalf("%s: no COVERAGE var", tmpl)
		}
		tt, err := template.New("COVERAGE").Funcs(funcs).Parse(expr)
		if err != nil {
			t.Fatalf("%s: COVERAGE does not parse as a Task template: %v", tmpl, err)
		}
		for _, c := range cases {
			var out strings.Builder
			if err := tt.Execute(&out, map[string]any{"FORGE_YAML": c.forgeYAML}); err != nil {
				t.Fatalf("%s: %q: %v", tmpl, c.forgeYAML, err)
			}
			if got := strings.TrimSpace(out.String()); got != c.want {
				t.Errorf("%s: forge.yaml %q: COVERAGE = %q, want %q", tmpl, c.forgeYAML, got, c.want)
			}
		}
	}
}
