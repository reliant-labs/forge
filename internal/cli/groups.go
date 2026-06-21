package cli

// This file blank-imports the dir-nested command-group subpackages so their
// init() runs and self-registers each group's command factory with
// internal/cli/factory. NewRootCmd ranges factory.Registered() to attach
// them. As commands migrate from the flat files in this package into group
// subpackages, add the group's import here.
//
// The import is one-directional: groups import internal/cli/factory (and may
// import internal/cli for shared helpers); internal/cli blank-imports the
// groups here. The registry indirection in the factory package is what keeps
// that from being an import cycle.
import (
	"github.com/reliant-labs/forge/internal/cli/factory"

	_ "github.com/reliant-labs/forge/internal/cli/backlog"
	_ "github.com/reliant-labs/forge/internal/cli/component"
	_ "github.com/reliant-labs/forge/internal/cli/debug"
	_ "github.com/reliant-labs/forge/internal/cli/pack"
)

// init wires internal/cli's heavy shared loaders into the factory so the
// dir-nested command groups can reach them as function values without
// importing internal/cli (which would create an import cycle — internal/cli
// blank-imports the groups above).
func init() {
	factory.SetProjectStoreLoader(loadProjectStore)
	factory.SetGenAPI(factory.GenAPI{
		// Full pipeline + bootstrap-only preset, each serialized under the
		// package-level generate mutex so the add group never touches a lock.
		RunPipeline: func(projectDir string) error {
			generateMu.Lock()
			defer generateMu.Unlock()
			return runGeneratePipeline(projectDir, false, false)
		},
		RunPipelineBootstrapOnly: func(projectDir string) error {
			generateMu.Lock()
			defer generateMu.Unlock()
			return runGeneratePipelineFlags(projectDir, pipelineFlags{Steps: "bootstrap-only"})
		},
		LoadServiceRegistry: func(projectDir string) (factory.ServiceRegistry, error) {
			reg, err := loadServiceRegistry(projectDir)
			if err != nil {
				return nil, err
			}
			return serviceRegistryAdapter{reg}, nil
		},
		ServiceRegistryRelPath: serviceRegistryRelPath,
		IsConnectServiceConfig: isConnectServiceConfig,
		WriteScenariosIndex:    writeScenariosIndex,
		RunPackageNew:          runPackageNew,
	})
}

// serviceRegistryAdapter exposes the internal *serviceRegistry to the add
// group through the factory.ServiceRegistry interface, mapping the field +
// state methods onto the narrow exported contract.
type serviceRegistryAdapter struct{ reg *serviceRegistry }

func (a serviceRegistryAdapter) Exists() bool                { return a.reg.Exists }
func (a serviceRegistryAdapter) Registered(name string) bool { return a.reg.registered(name) }
func (a serviceRegistryAdapter) Tombstoned(name string) bool {
	return a.reg.state(name) == registrationTombstoned
}
