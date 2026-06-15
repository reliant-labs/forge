// Package cli — hideDevFlags, the user-vs-maintainer CLI surface split.
//
// Commands like `forge lint` and `forge generate` accumulate flags for
// two very different audiences: project users (the 5-7 flags they
// actually reach for) and forge maintainers / debugging agents (wiring
// audits, pipeline narrowing, migration escape hatches). Showing all of
// them in --help buries the user-facing surface. hideDevFlags marks the
// maintainer set Hidden — fully functional, just invisible in --help —
// and registers a visible --help-dev flag that lists exactly the hidden
// set, so the flags stay discoverable in one place.
//
// Rule of thumb when adding a new flag: if a project user (not a forge
// developer) would reach for it in a normal week, it stays visible;
// otherwise add it to the command's hideDevFlags call. The surface test
// in help_surface_test.go pins the visible set so every new flag must
// consciously pick a side.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// hideDevFlags marks the named flags Hidden on cmd and adds a visible
// --help-dev flag that prints the hidden (maintainer/debug) flag set
// and exits 0 without running the command. Hidden flags keep working
// exactly as before — only --help visibility changes.
//
// Must be called after cmd.RunE and all flags are set. Panics on an
// unknown flag name: that's a programmer error at command-construction
// time, and panicking keeps a typo from silently leaving a dev flag
// visible.
func hideDevFlags(cmd *cobra.Command, names ...string) {
	var helpDev bool
	cmd.Flags().BoolVar(&helpDev, "help-dev", false, "List maintainer/debug flags hidden from --help")

	for _, name := range names {
		if err := cmd.Flags().MarkHidden(name); err != nil {
			panic(fmt.Sprintf("hideDevFlags(%s): %v", cmd.Name(), err))
		}
	}

	inner := cmd.RunE
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if helpDev {
			printDevFlags(c)
			return nil
		}
		if inner == nil {
			return nil
		}
		return inner(c, args)
	}
}

// printDevFlags writes every hidden flag of cmd (name + usage) to the
// command's stdout. Deprecated flags are annotated rather than skipped
// so --help-dev is the one complete inventory of the off-menu surface.
func printDevFlags(cmd *cobra.Command) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Maintainer/debug flags for \"forge %s\" (hidden from --help, fully functional):\n\n", cmd.Name())
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if !f.Hidden {
			return
		}
		usage := f.Usage
		if f.Deprecated != "" {
			usage += " (DEPRECATED: " + f.Deprecated + ")"
		}
		if f.Value.Type() == "bool" {
			fmt.Fprintf(out, "      --%s\n          %s\n", f.Name, usage)
		} else {
			fmt.Fprintf(out, "      --%s %s\n          %s\n", f.Name, f.Value.Type(), usage)
		}
	})
}
