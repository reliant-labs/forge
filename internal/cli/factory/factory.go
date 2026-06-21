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
}

// New returns a Factory wired to the real process streams. Tests construct a
// Factory literal with bytes.Buffer fields instead.
func New() *Factory {
	return &Factory{Out: os.Stdout, Err: os.Stderr, In: os.Stdin}
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
