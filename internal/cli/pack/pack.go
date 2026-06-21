// Package pack holds the `forge pack` command group — manage installable packs
// (list / install / remove / info).
//
// Dir-nested command group (the devspace idiom): the parent newCmd assembles
// the subcommands in the sibling files; the shared feature gate + small util
// stay in this file. init() self-registers the group with internal/cli/factory
// so a blank import from internal/cli/groups.go attaches it to the root.
//
// Shared internal/cli helpers are reached without importing internal/cli:
// project config via the factory's LoadProjectStore function value, and
// ProjectRoot / ErrProjectConfigNotFound / Name via internal/cli/cmdutil.
package pack

import (
	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cli/factory"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/installkit"
)

func init() { factory.Register(newCmd) }

// packsFeatureGate is the single feature gate every `forge pack <sub>`
// RunE invokes before doing real work. A LoadProjectStore failure (no
// forge.yaml, project not initialised) passes through unchanged so the
// existing "you must be in a forge project" UX is preserved when the user is
// outside any project.
func packsFeatureGate(f *factory.Factory) error {
	store, err := f.LoadProjectStore()
	if err != nil {
		// Outside a forge project: let `forge pack list` and friends
		// fall through so the existing "no forge.yaml" messaging
		// surfaces from the consumer code path. Returning the error
		// here would mask that signal behind a feature error.
		if err == cmdutil.ErrProjectConfigNotFound {
			return nil
		}
		return err
	}
	if !store.Features().PacksEnabled() {
		return config.DisabledFeatureError(config.FeaturePacks)
	}
	return nil
}

func newCmd(f *factory.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pack",
		Short: "Manage installable packs",
		Long: `Manage installable packs — pre-built, opinionated implementations
that add real, working code for specific concerns (auth, payments, etc.).

Subcommands:
  forge pack list              List available packs
  forge pack add <name>        Install a pack into the project (alias: install)
  forge pack remove <name>     Remove a pack from the project (alias: uninstall)`,
	}

	cmd.AddCommand(newListCmd(f))
	cmd.AddCommand(newInstallCmd(f))
	cmd.AddCommand(newRemoveCmd(f))
	cmd.AddCommand(newInfoCmd(f))

	return cmd
}

// indexByte returns the first index of c in s, or -1 if absent. Thin
// shim over installkit.FirstByteIndex to keep the original two-line
// CLI callsites readable.
func indexByte(s string, c byte) int {
	return installkit.FirstByteIndex(s, c)
}
