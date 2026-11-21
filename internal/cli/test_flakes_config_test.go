package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// forge.yaml's tests.flakes.issues is the default of `forge test flakes
// --issues`; outside a project, or with the key absent, it is off.
func TestTestFlakesIssuesFollowForgeYAML(t *testing.T) {
	for _, c := range []struct {
		name, yaml string
		want       bool
	}{
		{"on", "tests:\n  flakes:\n    issues: true\n", true},
		{"off", "tests:\n  flakes:\n    issues: false\n", false},
		{"absent", "tests:\n  coverage: true\n", false},
		{"no forge.yaml", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := flakesModule(t)
			if c.yaml != "" {
				body := "name: m\nmodule_path: example.com/m\n" + c.yaml
				if err := os.WriteFile(filepath.Join(root, "forge.yaml"), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(root)
			if got := testFlakesIssuesConfigured(); got != c.want {
				t.Errorf("testFlakesIssuesConfigured() = %v; want %v", got, c.want)
			}
		})
	}
}
