package add

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/cli/factory"
	"github.com/reliant-labs/forge/internal/config"
)

// markServiceProject makes dir derive to the "service" kind by stamping a
// real service artifact — the pkg/app composition root — since forge derives
// kind from the project's real sources (KCL tree, service registry, handler
// impls, protos), not a components.json manifest. The variadic components are
// ignored: the inventory is introspected from real sources at load, not
// authored. Named after its effect; the comps parameter is retained so the
// many call sites don't churn.
func writeComponentsJSON(t *testing.T, dir string, _ ...config.ComponentConfig) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "app"), 0o755); err != nil {
		t.Fatalf("mark service project (mkdir pkg/app): %v", err)
	}
}

// testFactory returns a Factory whose GenAPI fields are harmless stubs.
// The add white-box tests exercise validation / scaffold / forge.yaml
// logic and the cobra command surface — none of them drive a real
// generate pipeline — so the pipeline closures are no-ops and the
// registry loader returns an empty (fail-open) registry. Tests that need
// a specific Gen behavior override the relevant field on the returned
// value.
func testFactory() *factory.Factory {
	return &factory.Factory{
		Gen: factory.GenAPI{
			RunPipeline:              func(string) error { return nil },
			RunPipelineBootstrapOnly: func(string) error { return nil },
			LoadServiceRegistry: func(string) (factory.ServiceRegistry, error) {
				return stubRegistry{}, nil
			},
			ServiceRegistryRelPath: "pkg/app/services.go",
			IsConnectServiceConfig: func(config.ComponentConfig) bool { return true },
			WriteScenariosIndex:    func(string) error { return nil },
		},
	}
}

// stubRegistry is the fail-open registry the test factory hands back:
// everything reads as registered, nothing tombstoned — the pre-migration
// default, which keeps validation paths that consult the registry from
// rejecting in unit tests that don't set up a services.go.
type stubRegistry struct{}

func (stubRegistry) Exists() bool           { return false }
func (stubRegistry) Registered(string) bool { return true }
func (stubRegistry) Tombstoned(string) bool { return false }
