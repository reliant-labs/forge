package generator_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// The image registry is DECLARED in each env's KCL as a literal. A scaffolded
// env must never route it through a render option (`option("registry")`), the
// retired forge.registry helper, or a comment telling CI to pass `-D registry=`.
func TestScaffoldDeclaresEachEnvRegistryAsALiteral(t *testing.T) {
	registryLine := regexp.MustCompile(`(?m)^\s*registry = "([^"]+)"$`)
	for name, tc := range map[string]struct {
		module string
		shared bool
		cloud  string
	}{
		"github module":         {module: "github.com/Acme/shop", cloud: "ghcr.io/acme"},
		"non-github module":     {module: "example.com/shop", cloud: "ghcr.io/OWNER"},
		"shared-binary project": {module: "github.com/acme/shop", shared: true, cloud: "ghcr.io/acme"},
	} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			g := generator.NewProjectGenerator("shop", tmp, tc.module)
			g.Kind = config.ProjectKindService
			g.ApplyKindFeatureDefaults(config.ProjectKindService)
			if tc.shared {
				g.Binary = "shared"
			}
			if err := g.Generate(); err != nil {
				t.Fatalf("Generate: %v", err)
			}
			for env, want := range map[string]string{"dev": "localhost:5050", "staging": tc.cloud, "prod": tc.cloud} {
				raw, err := os.ReadFile(filepath.Join(tmp, "deploy", "kcl", env, "main.k"))
				if err != nil {
					t.Fatalf("read %s/main.k: %v", env, err)
				}
				body := string(raw)
				m := registryLine.FindStringSubmatch(body)
				if m == nil || m[1] != want {
					t.Errorf("%s/main.k: want the literal declaration `registry = %q`, got %v", env, want, m)
				}
				for _, bad := range []string{"forge.registry", `option("registry")`, "-D registry", "$REGISTRY"} {
					if strings.Contains(body, bad) {
						t.Errorf("%s/main.k carries %q — the registry is a literal the env declares, nothing overrides it", env, bad)
					}
				}
			}
		})
	}
}
