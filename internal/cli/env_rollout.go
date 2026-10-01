package cli

// `forge env rollout <env>`: where a promotion's rollout has got to, right now
// (hosted-deploy-primitives §3.5, task F7).
//
// It IS `forge env wait --timeout 0` — one GetRollout read, never blocking —
// and is implemented as exactly that: the same options, the same report, the
// same renderer and the same exit table, with Once set. A second
// implementation of "what phase is this rollout in" would be a second opinion,
// and §3.2's whole point is that the phase is computed in one place.

import "github.com/spf13/cobra"

func newEnvRolloutCmd() *cobra.Command {
	var opts envWaitOptions
	cmd := &cobra.Command{
		Use:   "rollout <environment>",
		Short: "Show where a promotion's rollout has got to, right now (one read, never blocks)",
		Long: `Read a hosted environment's rollout ONCE and report its phase.

The same answer ` + "`forge env wait`" + ` polls for, read a single time: per pinned workload,
the promoted digest against the one the control plane observes, and the
rollout's phase. It never blocks — it is ` + "`forge env wait <env> --timeout 0`" + `.

WHICH PROMOTION: by default the environment's current one. --promotion names
one exactly; --release refuses unless the current promotion binds that version.

Exit codes (the same table ` + "`env wait`" + ` uses):
  0  SUCCEEDED
  1  DEGRADED — a pinned workload is not serving
  2  could not determine — unobservable, unreachable, or the env does not
     converge promotions
  5  still PENDING / PROGRESSING / STABILIZING (not finished, not failed)
  6  SUPERSEDED — a newer promotion replaced this one

Examples:
  forge env rollout prod
  forge env rollout prod --json | jq -r '.workloads[] | "\(.name) \(.phase)"'`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Once = true
			return runEnvWaitForCmd(cmd.Context(), args[0], opts)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.PromotionID, "promotion", "", "Promotion id to read (default: the env's current promotion)")
	flags.StringVar(&opts.Release, "release", "", "Refuse unless the current promotion binds this release version (exit 6 if it moved on)")
	cmd.MarkFlagsMutuallyExclusive("promotion", "release")
	flags.BoolVar(&opts.IncludeUnpinned, "include-unpinned", false,
		"Let a degraded UNPINNED workload (a database, a third-party image) count toward the phase")
	flags.BoolVar(&opts.JSON, "json", false, "Emit machine-readable JSON (same exit codes)")
	return cmd
}
