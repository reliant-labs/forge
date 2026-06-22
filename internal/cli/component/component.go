// Package component holds the `forge component` command group — list, search,
// and install UI components from forge's built-in component library.
//
// It is the first dir-nested command group in forge's own CLI (the devspace
// idiom forge ships in generated apps). The parent newCmd assembles the
// subcommands defined in this package's sibling files (list.go, search.go,
// install.go); init() self-registers the group with internal/cli/factory so a
// blank import from internal/cli/groups.go attaches it to the root without a
// group↔root import cycle.
package component

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/factory"
)

func init() { factory.Register(newCmd) }

// newCmd builds the `component` parent command and attaches its subcommands.
func newCmd(f *factory.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "component",
		Short: "Manage UI components from the component library",
		Long:  "List, search, and install UI components from Forge's built-in component library.",
	}
	cmd.AddCommand(newListCmd(f))
	cmd.AddCommand(newSearchCmd(f))
	cmd.AddCommand(newInstallCmd(f))
	return cmd
}

// detectComponentsDir looks for a frontends/*/src/components/ui/ directory
// relative to the current working directory.
func detectComponentsDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	// Check frontends directory
	frontendsDir := filepath.Join(cwd, "frontends")
	entries, err := os.ReadDir(frontendsDir)
	if err != nil {
		return ""
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(frontendsDir, e.Name(), "src", "components", "ui")
		if info, err := os.Stat(filepath.Join(frontendsDir, e.Name(), "src")); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}
