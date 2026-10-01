package cli

// `forge env status [environment...]` — THE one read view of an environment
// (ADR docs/adr/env-verbs.md, task V4).
//
// WHY ONE COMMAND. Reading an environment used to take six verbs — `env
// status`, `env verify`, `env wait`, `env rollout`, `env topology`, `env
// history` — and they were views of a single question, split by which half of
// the answer each happened to own. A reader had to know, before they could
// ask, that the bound release lived in `verify`, the rollout phase in
// `rollout`, the runtime ports in `status`, and the promotion that caused all
// of it in `history`. Worse, two of the six were literal duplicates:
// `env rollout` WAS `env wait --timeout 0`.
//
// So they are modes of one command now, and the modes are the questions a
// reader actually has:
//
//	forge env status              every environment (the old `env topology`)
//	forge env status prod         prod, right now: runtime AND release
//	forge env status prod --wait  block until the rollout settles (old `env wait`)
//	forge env status prod --history  what has prod run (old `env history`)
//
// EACH MODE REACHES THE SAME CODE THE DELETED VERB CALLED. Nothing was
// reimplemented in the move: --wait is runEnvWait, --history is runEnvHistory,
// the all-envs view is runEnvTopology, and the release half is the old
// verify's runEnvStatusRelease. That is what makes "every exit code is
// unchanged" a property of the wiring rather than a promise.
//
// EXIT CODES COME FROM THE HALF THAT OWNS THEM, and the one subtlety worth
// knowing is that the two halves of the default view are NOT symmetric:
//
//   - The RUNTIME half reports and never fails. "The app is down" is a state
//     this command must be able to print — a status verb that exited non-zero
//     for it would be unusable as the one read view, and the runtime checks
//     have always had that contract.
//   - The RELEASE half's verdict IS the command's exit code (0 / 1 / 2),
//     because that half makes a CLAIM: the env is or is not running what it
//     declares. 1 is "we looked and it is wrong", 2 is "we could not look".
//
// --wait keeps 0/1/5/6/2 and --history keeps 0/1/2, both untouched.

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// newEnvStatusCmd is `forge env status [environment...]`.
//
// The env is OPTIONAL — that is the one arity change the merge makes, and it
// is what absorbs the old `env topology`: with no env there is nothing to
// scope to, so the answer is every env. Several envs are accepted for the
// same reason topology accepted them, to include one that is bound in the
// ledger but not declared in this checkout.
func newEnvStatusCmd() *cobra.Command {
	var (
		jsonOut bool
		signal  string
		verbose bool

		// timeout serves two modes with DIFFERENT budgets, which is
		// why its default is the zero sentinel rather than either
		// one: a cluster read gets 60s (an unreachable cluster must
		// fail a CI job, not hang it) and a wait gets 15m (a cold
		// cloud rollout legitimately takes minutes). One flag cannot
		// declare two defaults, so unset resolves per mode below.
		timeout time.Duration

		// --wait: the old `env wait`, whose options this fills.
		wait     bool
		waitOpts envWaitOptions

		// --history: the old `env history`.
		history bool
		histQ   historyQuery

		// The all-envs view's cluster reconciliation.
		verifyClusters bool
	)

	cmd := &cobra.Command{
		Use:   "status [environment...]",
		Short: "The one read view of an environment: bound release, rollout, health, verify, gates, ledger",
		Args:  cobra.ArbitraryArgs,
		Long: `Report everything about an environment in one read.

  forge env status                 every environment, and how far behind each is
  forge env status prod            prod right now: runtime AND release
  forge env status prod --wait     block until the rollout settles
  forge env status prod --history  prod's promotion ledger, newest first

WHAT ONE ENV'S REPORT CARRIES:

  * the BOUND RELEASE — the version this env's promotion ledger declares, and
    when it was promoted (promote time, NOT deploy time; that gap is what the
    verify half exists to measure);
  * VERIFY — the digests the env is actually RUNNING against the ones the
    binding froze, per declared image, in five states: match / drift /
    missing / untagged / unreachable. A hosted env is read through its
    control plane's observer, because forge cannot read a hosted cluster;
  * the ROLLOUT PHASE per workload, as the control plane computes it;
  * RUNTIME HEALTH — every host service and frontend with its URL, log file
    and whether a listener is accepting right now, plus the compose infra,
    the app's /healthz + /readyz, pprof and the telemetry backends;
  * GATES — the evidence recorded for the current promotion;
  * LEDGER FRESHNESS — whether this checkout's copy of the promotion log is
    the newest one.

THE TWO HALVES ARE NOT SYMMETRIC ABOUT FAILURE, on purpose. Runtime health
REPORTS: "the app is down" is a state this command must be able to print, so
it never changes the exit code. The release half makes a CLAIM — the env is or
is not running what it declares — so its verdict is the exit code.

` + "`--wait`" + ` blocks until the rollout of a promotion finishes, and reports whether
it STAYED up past the stability window. It is pinned to ONE promotion, never
to "whatever the env declares now", so a hotfix promoted mid-wait reports
SUPERSEDED rather than succeeding on bytes it was never asked about.
` + "`--wait --timeout 0`" + ` reads the phase ONCE and never blocks.

` + "`--history`" + ` pages the promotion ledger with a keyset cursor: pass the previous
page's ` + "`next_before`" + ` as --before, and stop on an EMPTY next_before rather than
on a short page.

EXIT CODES — a pipeline branches on these directly:

  0  everything declared is running (or nothing is declared)
  1  we looked and it is WRONG — an image drifted or is missing; with --wait,
     a workload is DEGRADED
  2  we could not DETERMINE — a cluster or control plane was unreachable, a
     credential was refused, the thing is unobservable, or this checkout's
     promotion ledger is stale
  5  --wait only: TIMED OUT while still pending / progressing / stabilizing.
     The rollout was progressing, so retry the wait; do not re-promote
  6  --wait only: SUPERSEDED — a newer promotion replaced the one waited on

1 and 2 are separate because CI must tell a bad release from a broken control
plane; a gate that reports both with one code gets switched off the first week
it is wrong about one of them. 5 and 6 are deliberately not 1: "we never saw
this finish" and "the release was overtaken" are not "the release is bad".

A STALE FILE LEDGER IS EXIT 2. .forge/promotions/<env>.jsonl is committed to
git, so a checkout that has not pulled compares the cluster against an OLDER
promotion — a fine deploy reads as DRIFT, and a deploy that never happened can
read as MATCH. Neither is evidence, so the verdict is "could not determine"
with the fix. AHEAD (a release recorded here, not yet merged) is noted, not
failed.

--json emits ONE document with the same verdict and IDENTICAL exit codes;
` + "`ok`" + ` is false exactly when the process exits non-zero.

Examples:
  forge env status                                  # the whole topology, offline
  forge env status prod                             # did prod receive its release?
  forge env status prod --wait                      # gate a release on the rollout
  forge env status prod --wait --timeout 0 --json   # where is it NOW? one read
  forge env status prod --history --limit 1 --json | jq -r '.promotions[0].id'
  forge env status prod --json | jq -r '.images[] | select(.state == "drift")'`,
		// The command's findings ARE its output; a cobra usage dump on a
		// drift failure would bury them under the flag list.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --wait and --history are two different reads of one env,
			// and asking both at once has no answer. Refused rather
			// than silently preferring one.
			if wait && history {
				return fmt.Errorf("--wait and --history are two different reads: --wait blocks on the CURRENT rollout, --history pages PAST promotions. Pass one")
			}
			// No env: every env. There is nothing to block on and no
			// single ledger to page, so the two per-env modes are
			// refused here rather than quietly ignored.
			if len(args) == 0 {
				if wait || history {
					return fmt.Errorf("--wait and --history need an environment: `forge env status` with no environment is the all-environments view")
				}
				return runEnvTopology(cmd.Context(), args, envTopologyOptions{
					JSON:       jsonOut,
					Verify:     verifyClusters,
					Timeout:    timeout,
					ProjectDir: projectDirForKCL(),
					Lister:     kubectlImageLister{},
					Resolver:   kclTargetResolver{},
				})
			}
			// Several envs is the all-envs view, scoped. It is how an
			// env bound in the ledger but absent from this checkout is
			// included, which the per-env modes cannot do.
			if len(args) > 1 {
				if wait || history {
					return fmt.Errorf("--wait and --history read ONE environment, got %d", len(args))
				}
				return runEnvTopology(cmd.Context(), args, envTopologyOptions{
					JSON:       jsonOut,
					Verify:     verifyClusters,
					Timeout:    timeout,
					ProjectDir: projectDirForKCL(),
					Lister:     kubectlImageLister{},
					Resolver:   kclTargetResolver{},
				})
			}

			env := args[0]
			switch {
			case wait:
				// `--timeout 0` means ONE READ, never block — the
				// single-shot mode the old `forge env rollout` was.
				//
				// Those two have to be told apart, and a zero value
				// cannot do it: Timeout's zero IS the unset value, so
				// "0" and "not given" arrive identically. cobra's
				// Changed is the only thing that knows the difference,
				// and it is only available here, where the flag set
				// is. Reading it wrong in either direction is bad in
				// its own way — an unset flag becoming a single read
				// would turn every plain --wait into a non-blocking
				// poll, and an explicit 0 becoming 15m would make the
				// snapshot block for a quarter of an hour.
				changed := cmd.Flags().Changed("timeout")
				switch {
				case changed && timeout == 0:
					waitOpts.Once = true
				case changed:
					waitOpts.Timeout = timeout
				default:
					waitOpts.Timeout = envWaitDefaultTimeout
				}
				waitOpts.JSON = jsonOut
				return runEnvWaitForCmd(cmd.Context(), env, waitOpts)
			case history:
				store, err := bindingStoreFor(cmd.Context(), projectDirForKCL(), env)
				if err != nil {
					return exitCodeError{code: exitUndetermined, msg: err.Error()}
				}
				return runEnvHistory(cmd.Context(), store, env, histQ, jsonOut, cmd.OutOrStdout())
			}
			return runEnvStatus(cmd.Context(), env, envStatusOptions{
				Timeout:  timeout,
				JSON:     jsonOut,
				Signal:   signal,
				Verbose:  verbose,
				Lister:   kubectlImageLister{},
				Resolver: kclTargetResolver{},
			})
		},
	}

	flags := cmd.Flags()
	flags.BoolVar(&jsonOut, "json", false, "Emit ONE machine-readable document (same exit codes as text mode)")

	// The runtime half.
	flags.StringVar(&signal, "signal", "", "Run only one runtime signal: app, metrics, traces, logs, profiles (default: all)")
	flags.BoolVarP(&verbose, "verbose", "v", false, "Show evidence for all runtime checks (not just failures)")

	// The release half, and the all-envs view's cluster read.
	// Default 0 is the UNSET sentinel, not a budget: the two modes resolve
	// it differently (60s for a cluster read, 15m for a wait) and one flag
	// cannot declare both. The help names both numbers so the default
	// shown as "0s" is not mistaken for "no timeout".
	flags.DurationVar(&timeout, "timeout", 0,
		fmt.Sprintf("Maximum time to spend reading the cluster (default %s). With --wait this is the whole wait budget instead (default %s); `--wait --timeout 0` reads the phase ONCE and never blocks",
			defaultEnvStatusReleaseTimeout, envWaitDefaultTimeout))
	flags.BoolVar(&verifyClusters, "verify", false,
		"All-environments view only: also read each environment's cluster and reconcile it against the ledger (slow, needs credentials)")

	// --wait: the old `forge env wait`, every flag unchanged.
	flags.BoolVar(&wait, "wait", false, "Block until the rollout settles, and prove it stayed up (exit 0/1/2/5/6)")
	flags.StringVar(&waitOpts.PromotionID, "promotion", "",
		"Promotion id to read (default: the env's current promotion). A CI retry passes the id the deploy returned")
	flags.StringVar(&waitOpts.Release, "release", "",
		"Refuse unless the promotion read binds this release version (exit 6 if it moved on). With --history, only promotions of this version")
	cmd.MarkFlagsMutuallyExclusive("promotion", "release")
	flags.DurationVar(&waitOpts.StableFor, "stable-for", 0,
		"--wait only: extra hold AFTER the phase reaches succeeded (default 0: the server's own stability window already applies)")
	flags.BoolVar(&waitOpts.FailFast, "fail-fast", false,
		"--wait only: exit 1 on the FIRST degraded observation instead of waiting out --timeout")
	flags.BoolVar(&waitOpts.IncludeUnpinned, "include-unpinned", false,
		"Let a degraded UNPINNED workload (a database, a third-party image) count too")
	flags.DurationVar(&waitOpts.Interval, "interval", envWaitDefaultInterval, "--wait only: poll cadence")
	flags.BoolVar(&waitOpts.WatchJSON, "watch-json", false,
		"--wait only: emit NDJSON, one line per phase change, as the rollout progresses")

	// --history: the old `forge env history`, every flag unchanged.
	flags.BoolVar(&history, "history", false, "Page the environment's promotion ledger, newest first")
	flags.IntVar(&histQ.Limit, "limit", defaultHistoryLimit, fmt.Sprintf("--history only: entries per page (1–%d)", maxHistoryLimit))
	flags.StringVar(&histQ.Before, "before", "",
		"--history only: return entries older than this promotion id (the previous page's next_before)")

	// --wait's budget and the cluster-read budget are the same flag with
	// two meanings, which only works because they are never both in play.
	cmd.MarkFlagsMutuallyExclusive("wait", "history")
	cmd.MarkFlagsMutuallyExclusive("wait", "verify")
	cmd.MarkFlagsMutuallyExclusive("history", "verify")

	return cmd
}

// runEnvStatus is the DEFAULT view of one env: runtime first, then release.
//
// Runtime comes first because it is the answer most invocations want and it
// is the cheap half — no cluster credentials, no ledger. The release half
// reads last and owns the verdict.
//
// The two halves are deliberately sequential rather than concurrent. They
// print one after the other and a reader follows them in order; running them
// in parallel would interleave two progress streams for no gain on a command
// whose slow part is one cluster read.
func runEnvStatus(ctx context.Context, env string, opts envStatusOptions) error {
	// The runtime half REPORTS. Its error is returned only for a usage
	// mistake (a mistyped --signal), never for a down stack — see the file
	// header on why the two halves are asymmetric about failure.
	if err := runUpServices(ctx, env, opts.JSON, opts.Signal, opts.Verbose); err != nil {
		return err
	}
	if !opts.JSON {
		fmt.Println()
	}
	// The release half owns the exit code.
	return runEnvStatusRelease(ctx, env, opts)
}
