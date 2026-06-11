// `forge generate accept-fork <path>...` — DEPRECATED alias for
// `forge disown`.
//
// The fork concept is gone: there is no forked-but-maybe-reconciled-
// later limbo any more, only forge-owned (Tier-1) and user-owned
// (Tier-2), with `forge disown` as the one-way door between them.
// accept-fork survives one release so existing scripts and muscle
// memory don't break overnight; it simply forwards to the disown flow
// (and therefore now REQUIRES --reason, like disown does).
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newAcceptForkCmd() *cobra.Command {
	var (
		dryRun bool
		reason string
	)

	cmd := &cobra.Command{
		Use:        "accept-fork <path>...",
		Short:      "DEPRECATED: alias for `forge disown`",
		Deprecated: "use `forge disown <path>... --reason \"<why>\"` instead — forks were removed; disowning is the one-way transfer to user ownership.",
		Long: `DEPRECATED alias for ` + "`forge disown`" + `. The fork state no longer exists;
this command forwards to the disown flow and will be removed in the next
release.

Run ` + "`forge disown <path>... --reason \"<why>\"`" + ` instead.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "⚠️  DEPRECATED: `forge generate accept-fork` is now an alias for `forge disown` and will be removed in the next release.")
			fmt.Fprintln(os.Stderr, "   Use `forge disown <path>... --reason \"<why>\"`.")
			return runDisown(args, reason, dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would change without writing .forge/checksums.json")
	cmd.Flags().StringVar(&reason, "reason", "", "WHY forge's generated code couldn't express what you needed (required; recorded per path in .forge/friction.jsonl)")

	return cmd
}
