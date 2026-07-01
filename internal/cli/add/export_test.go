package add

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/cli/factory"
	"github.com/reliant-labs/forge/internal/config"
)

// writeComponentsJSON drops a components.json at dir holding the given
// components. A copy of internal/cli's api_test.go helper, duplicated here
// because the `add` group is its own package and the cli test helper does
// not cross the package boundary. Passing zero components still writes
// `{"components":[]}` so the project derives to service kind.
func writeComponentsJSON(t *testing.T, dir string, comps ...config.ComponentConfig) {
	t.Helper()
	data, err := config.MarshalComponentsJSON(comps)
	if err != nil {
		t.Fatalf("marshal components.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.ComponentsFileName), data, 0o644); err != nil {
		t.Fatalf("write components.json: %v", err)
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
