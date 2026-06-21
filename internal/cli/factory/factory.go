// Package factory carries the shared dependency set ("the factory") threaded
// through forge's own CLI command tree, plus the command REGISTRY that lets
// dir-nested command-group subpackages (internal/cli/add, internal/cli/lint,
// ...) attach to the root without a group↔root import cycle.
//
// This mirrors the devspace/argo-cd idiom we ship in generated apps
// (internal/templates/project/cmd-tree-root.go.tmpl): a small package that
// owns Deps + a registry of CmdFactory funcs. Group subpackages import THIS
// package for Deps and self-register their command constructor via init();
// the root package (internal/cli) blank-imports the groups so their init()
// runs, then ranges the registry to assemble the tree. The indirection is
// what keeps the dependency one-directional (group → factory), breaking the
// cycle that a direct group→root import would create.
//
// Why a dedicated package (not internal/cli itself): the group subpackages
// need to import the factory/registry, and they ALSO need shared logic
// helpers that live in internal/cli (loadProjectStore, runGeneratePipeline,
// ...). If the registry lived in internal/cli, the groups importing it would
// import internal/cli — and internal/cli blank-imports the groups, a cycle.
// Pulling Deps + registry into their own leaf package breaks it.
package factory

import (
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/projectstore"
)

// Factory is the shared dependency set carried into command constructors so
// commands are testable (writers can be redirected) and root setup lives in
// one place. It is intentionally small: forge's command logic calls
// package-level helpers in internal/cli directly, so the factory carries the
// I/O surface and is the vehicle for the registration pattern rather than a
// god-object of injected collaborators.
type Factory struct {
	// Out is the writer for user-facing stdout output.
	Out io.Writer
	// Err is the writer for diagnostics / stderr output.
	Err io.Writer
	// In is the reader for interactive prompts.
	In io.Reader

	// LoadProjectStore loads forge.yaml (walking up from cwd) into a
	// ProjectStore — the single read+mutate surface. The heavy config-parsing
	// logic lives in internal/cli (config.go); the factory carries it as a
	// function value so group subpackages can read project config without
	// importing internal/cli (which would cycle: internal/cli blank-imports
	// the groups). internal/cli wires this in factory.New's caller via
	// SetProjectStoreLoader.
	LoadProjectStore func() (projectstore.ProjectStore, error)

	// Gen is the clean, narrow surface over the generate pipeline + service
	// registry that the dir-nested `add` group calls. The heavy
	// implementation (the ~80-file generate pipeline, the services.go AST
	// parser) lives in internal/cli; the factory carries it as function
	// values so internal/cli/add can trigger codegen and read the
	// registration view without importing internal/cli (which would cycle —
	// internal/cli blank-imports the groups). internal/cli registers the
	// concrete implementation via SetGenAPI from its init().
	Gen GenAPI
}

// GenAPI is the exported codegen + service-registry surface the `add`
// command group depends on. Each field is a narrow, well-named entry into
// logic that stays in internal/cli; the group never reaches package cli
// internals directly. Mutex serialization of the pipeline (generateMu) is
// owned by the internal/cli-side closures, so callers here never touch a
// lock.
type GenAPI struct {
	// RunPipeline runs the FULL generate pipeline for the project rooted at
	// projectDir (the equivalent of `forge generate`), serialized under the
	// internal/cli generate mutex. Used by `add service` / `add operator`.
	RunPipeline func(projectDir string) error

	// RunPipelineBootstrapOnly runs the generate pipeline narrowed to the
	// "bootstrap-only" step preset (regenerates pkg/app/{bootstrap,testing,
	// migrate}.go and nothing else), serialized under the generate mutex.
	// Used by `add worker`.
	RunPipelineBootstrapOnly func(projectDir string) error

	// LoadServiceRegistry parses the user-owned pkg/app/services.go and
	// returns the registration view. Used by `add service` / `add webhook` /
	// `add rpc` to print accurate registration nudges and to gate the
	// types-only (tombstoned) case.
	LoadServiceRegistry func(projectDir string) (ServiceRegistry, error)

	// ServiceRegistryRelPath is the project-relative path of the user-owned
	// registration file (pkg/app/services.go), surfaced so the group's
	// messages name it without duplicating the constant.
	ServiceRegistryRelPath string

	// IsConnectServiceConfig reports whether a component is a Connect
	// service (vs worker/cron/operator/binary). Used by `add webhook` to
	// gate the registration nudge to serving binaries.
	IsConnectServiceConfig func(c config.ComponentConfig) bool

	// WriteScenariosIndex regenerates the frontend mock-scenario barrel
	// (scenarios/index). Used by `add scenario`.
	WriteScenariosIndex func(scenariosDir string) error

	// RunPackageNew is the `forge package new` RunE, reused verbatim by
	// `add package` and `add adapter` (which pre-sets --type).
	RunPackageNew func(cmd *cobra.Command, args []string) error
}

// ServiceRegistry is the narrow registration view the `add` group reads.
// It mirrors the relevant subset of internal/cli's serviceRegistry: a
// service is REGISTERED (this binary serves it), TOMBSTONED (deliberately
// retired — types-only), or neither (unlisted). The concrete value is
// adapted on the internal/cli side.
type ServiceRegistry interface {
	// Exists reports whether the registration file is present. When false,
	// every service reads as registered (fail-open pre-migration behavior).
	Exists() bool
	// Registered reports whether this binary serves the named service.
	Registered(name string) bool
	// Tombstoned reports whether the named service is deliberately retired
	// (mentioned in a comment but with no serviceRow reference).
	Tombstoned(name string) bool
}

// genAPI is the GenAPI internal/cli registers so New can populate every
// Factory it builds. Injected (not imported) to keep the factory a leaf.
var genAPI GenAPI

// SetGenAPI registers the codegen + registry surface. internal/cli calls
// this from an init() so it is set before any Factory is built.
func SetGenAPI(g GenAPI) { genAPI = g }

// projectStoreLoader is the loader internal/cli registers so New can populate
// every Factory it builds. Injected (rather than imported) to keep the
// factory package a leaf that the command groups can depend on.
var projectStoreLoader func() (projectstore.ProjectStore, error)

// SetProjectStoreLoader registers the project-store loader. internal/cli calls
// this from an init() so the loader is set before any Factory is built.
func SetProjectStoreLoader(load func() (projectstore.ProjectStore, error)) {
	projectStoreLoader = load
}

// New returns a Factory wired to the real process streams and the registered
// project-store loader. Tests construct a Factory literal with bytes.Buffer
// fields (and their own loader) instead.
func New() *Factory {
	return &Factory{
		Out:              os.Stdout,
		Err:              os.Stderr,
		In:               os.Stdin,
		LoadProjectStore: projectStoreLoader,
		Gen:              genAPI,
	}
}

// CmdFactory builds one top-level command from the shared factory. Group
// subpackages register a CmdFactory (their newXCmd) via Register; the root
// ranges the registry to assemble the tree.
type CmdFactory func(f *Factory) *cobra.Command

// commandFactories is the registry the group subpackages populate at init().
var commandFactories []CmdFactory

// Register adds a top-level command factory to the registry. Group
// subpackages call this from their init() so a blank import of the group is
// enough to attach the command to the root.
func Register(c CmdFactory) { commandFactories = append(commandFactories, c) }

// Registered returns the registered command factories in registration order.
// The root command builder ranges this to AddCommand each one.
func Registered() []CmdFactory { return commandFactories }
