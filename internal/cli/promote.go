package cli

import (
	"context"
	"fmt"
	"os/user"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/pkg/release"
)

// newPromoteCmd is `forge env promote <version> --to <env>`: bind an env to a
// release. This is the "promote, don't rebuild" half of the build-once model.
// It reads the release `forge env build --release` cut, freezes each image's
// digest, and APPENDS one entry to the env's promotion ledger — the project's
// .forge/promotions/<env>.jsonl, or the control plane the env's KCL declares.
// No build runs; the bytes that were cut as <version> are, by construction,
// the bytes the env will deploy.
func newPromoteCmd() *cobra.Command {
	var (
		toEnv         string
		dryRun        bool
		jsonOut       bool
		note          string
		actor         string
		expectCurrent string
		expectUnbound bool
		supersede     bool
		follow        promoteFollowOptions
		gates         []string
		from          promoteFromOptions
		run           runOptions
	)

	cmd := &cobra.Command{
		Use:   "promote <version> --to <env>",
		Short: "Bind an environment to a release (build once, promote — no rebuild)",
		Long: `Bind an environment to an already-built release.

` + "`forge env build --release <version>`" + ` builds the env-agnostic images ONCE,
captures their content-addressed digests, and cuts a release. ` + "`forge env promote`" + `
advances that release to an environment BY REFERENCE: it appends one entry —
env, release, and the per-image digests frozen at this moment — to the env's
append-only promotion ledger. No image is rebuilt — the exact bytes cut as
<version> are what the env ships.

WHERE THE LEDGER LIVES is declared by the environment, not chosen by a flag:
an env whose KCL declares forge.ControlPlane records promotions on that control
plane; every other env records them in .forge/promotions/<env>.jsonl.

EVERY PROMOTE IS A NEW ENTRY, NOT AN EDIT. Re-promoting the release an env
already runs appends nothing; a CI retry is safe.

THERE IS NO ROLLBACK. Recovery is ROLL FORWARD: cut a release with the fix and
promote it. Binding an env to an OLDER release is still possible — it is an
ordinary promote — but it cannot undo the newer release: that release's
migrations stay applied and the data it wrote stays written, so the older code
runs against a schema it was never tested on. The plan labels such a move
` + "`direction: BEHIND`" + ` and says so; read it before you write it.

` + "`forge env deploy <env>`" + ` then pins those SAME digests, so every env promoted
to the same release deploys byte-identical images. This eliminates the per-env
rebuild that re-cross-compiles (and can drift arch/tag) for every environment.

SEE THE CHANGE BEFORE IT IS WRITTEN. --plan computes the ENTIRE change set and
writes nothing: the release the env runs now versus the one it would move to,
every image classified as unchanged / changed / added / removed (with both
digests where they differ), the git commits between the two releases, and —
the fact most worth reading twice — the DIRECTION. A promote to an older
release is reported as BEHIND rather than left for you to infer from version
numbers.

The plan and the real promote are computed by the SAME function, so the
preview cannot disagree with the write. --json emits it machine-readably, in
one document shape for both modes, with an ` + "`applied`" + ` field saying which one you
got.

PROMOTE SHIPS NOTHING. It moves a pointer. No image reaches any cluster until
` + "`forge env deploy <env>`" + ` runs, and ` + "`forge env verify <env>`" + ` is how you prove it
arrived. The plan says so on every invocation.

EVERY PROMOTE IS A COMPARE-AND-SET. The write asserts that the env is still on
the promotion the plan read (` + "`current.promotion_id`" + ` in --json), and is
REFUSED if someone else moved it since — so a hotfix that lands while a
pipeline waits for approval turns the pipeline red instead of being
overwritten. No flag is needed. --expect-current <id> replaces the planned
value with one captured earlier (e.g. when the approval was requested);
` + "`--expect-current unbound`" + ` (or --expect-unbound) asserts the env has never been
promoted. Re-promoting the release the env already runs is a no-op whatever
the expectation says, so a retried success is never a conflict.

Exit codes:
  0  promoted, or already on this release (no-op), or --plan
  1  failed: invalid input, unreadable ledger, release not found
  3  promotion_conflict — the env moved since the plan was read. Stop and
     look; retrying would overwrite what landed
  4  rollout_in_flight / environment_pinned — declined, nothing lost. Wait
     and retry, or pass --supersede (recorded) to replace an unfinished rollout
--json carries the same outcome: ` + "`applied: false`" + ` and a ` + "`refusal`" + ` object naming
what was expected and what is actually there.

Examples:
  forge env build prod --release v1.4.0 --push   # build once, cut the release (prod's declared registry)
  forge env promote v1.4.0 --to staging --plan            # what WOULD change (writes nothing)
  forge env promote v1.4.0 --to staging --plan --json     # the same, machine-readable
  forge env promote v1.4.0 --to staging                  # bind staging → v1.4.0
  forge env deploy staging                               # ships v1.4.0's digests
  forge env promote v1.4.0 --to prod                     # same digests advance to prod
  forge env deploy prod                                  # the bytes that passed staging
  forge env promote v1.3.0 --to prod --plan | grep BEHIND       # catch a backwards move
  ID=$(forge env promote v1.4.0 --to prod --plan --json | jq -r '.current.promotion_id // "unbound"')
  forge env promote v1.4.0 --to prod --expect-current "$ID"   # after approval: exit 3 if prod moved`,
		// At most one: --from (F5) can supply the version instead.
		Args: cobra.MaximumNArgs(1),
		// The change set IS the output; a cobra usage dump would bury it
		// under the flag list.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var version string
			if len(args) == 1 {
				version = args[0]
			}
			if toEnv == "" {
				return fmt.Errorf("--to <env> is required: name the environment to bind to release %q", version)
			}
			if expectUnbound {
				expectCurrent = expectUnboundLiteral
			}
			// The checkout and its releases are resolved HERE, once, and
			// passed down. runPromote and computePromotePlan both still
			// fall back when these are empty — that is what lets a test
			// state them — but the production path states them too, so
			// the fields carry a real value rather than only ever the
			// zero one a test overwrites.
			return runPromote(cmd.Context(), version, toEnv, promoteOptions{
				DryRun:        dryRun,
				JSON:          jsonOut,
				ProjectDir:    projectDirForKCL(),
				Note:          note,
				Actor:         actor,
				ExpectCurrent: expectCurrent,
				Supersede:     supersede,
				Follow:        follow,
				Gates:         gates,
				From:          from,
				Run:           run,
			})
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&toEnv, "to", "", "Environment to bind to the release (required)")
	flags.BoolVar(&dryRun, "plan", false, "Compute and print the full change set WITHOUT writing the binding")
	flags.BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON (same exit codes as text mode)")
	flags.StringVar(&note, "note", "", "Why — recorded on the ledger entry (most valuable on a promote that moves the env BEHIND)")
	flags.StringVar(&actor, "actor", "", "Name the automation recording this (e.g. ci); default is the local user")

	// Anti-stomp (§3.1). Every promote compare-and-sets against the plan's
	// read; these replace that value, they do not enable it.
	flags.StringVar(&expectCurrent, "expect-current", "",
		"Promotion id the env must still be on (default: the one the plan read); `unbound` = --expect-unbound. Exit 3 if it moved")
	flags.BoolVar(&expectUnbound, "expect-unbound", false, "Refuse (exit 3) unless the env has never been promoted")
	cmd.MarkFlagsMutuallyExclusive("expect-current", "expect-unbound")
	flags.BoolVar(&supersede, "supersede", false,
		"Promote even though the current promotion is still rolling out (recorded on the new entry); without it that is exit 4")

	// Follow-through (§3.2, task F3 — promote_wait.go).
	flags.BoolVar(&follow.Wait, "wait", false, "After promoting, wait for the rollout like `forge env wait` (exit 0/1/2/5/6)")
	flags.BoolVar(&follow.Deploy, "deploy", false, "After promoting, run the client-side deploy (for an env that does not converge promotions)")
	flags.DurationVar(&follow.Timeout, "timeout", 0, "Whole --wait budget (default 15m)")
	flags.BoolVar(&follow.FailFast, "fail-fast", false, "With --wait: exit 1 on the first DEGRADED observation")

	// Evidence (§3.3, task F4 — promote_gates.go).
	flags.StringArrayVar(&gates, "gate", nil,
		"Pre-promote evidence frozen onto the entry: a gate JSON file, or name=…,status=passed|failed|skipped|error[,url=…] (repeatable)")

	// Source (§3.4, task F5 — promote_from.go).
	flags.StringVar(&from.Env, "from", "", "Promote exactly what this environment is running (same control plane only); the version may be omitted")
	flags.StringVar(&from.PromotionID, "from-promotion", "", "With --from: the source promotion captured earlier; refused (exit 3, source_moved) if the source moved")

	// Run identity (§3.A): --run-id / --run-url / --no-run, defaulted from CI.
	registerRunFlags(flags, &run)

	return cmd
}

// promoteOptions carries the flags and the seams into runPromote.
//
// The three seams (Bindings, Releases, Git) are injected for the same reason
// env verify injects its three: a test asserting that --plan writes nothing,
// or that a backwards move is reported as one, should be able to STATE the ledger
// and the git history rather than staging a project and a repository to imply
// them. Production leaves them nil and gets the real ones.
type promoteOptions struct {
	// DryRun computes the plan and stops. Nothing is written, exit 0.
	DryRun bool
	// JSON switches the RENDERING only. The plan is computed before either
	// renderer runs, so the two modes cannot disagree about what was found.
	JSON bool
	// ProjectDir is the checkout read from. Empty falls back to discovery.
	ProjectDir string
	// Note and Actor are recorded on the ledger entry.
	Note  string
	Actor string
	// ExpectCurrent overrides the compare-and-set expectation the plan
	// read: a promotion id, or expectUnboundLiteral. Empty = the plan's.
	ExpectCurrent string
	// Supersede admits a promote while the current rollout is in flight.
	Supersede bool
	// Follow, Gates and From are the hooks F3, F4 and F5 own, in
	// promote_wait.go, promote_gates.go and promote_from.go. Declared here
	// so promote.go — and its flag surface — has one owner.
	Follow promoteFollowOptions
	Gates  []string
	From   promoteFromOptions
	// Run is the run identity; resolved against the CI environment.
	Run runOptions
	// Bindings and Releases are the env's ledger. Nil resolves the env's
	// declared backend.
	Bindings bindingStore
	Releases releaseLedger
	Git      promoteGitReader
}

// runPromote computes the change set and — unless --plan was passed — applies
// it.
//
// PLAN THEN APPLY, ALWAYS, EVEN WITHOUT --plan. The real promote takes the
// identical path a dry run does and then writes; it does not have a second,
// leaner implementation. That is what makes the preview trustworthy: there is
// no code the write executes that the plan did not describe. It also means the
// success output is the change set rather than a digest dump, so the operator
// who skipped the preview still sees what moved.
//
// Resolving the digests at promote time (and snapshotting them into the
// binding) is unchanged and still deliberate: it makes "the bytes that passed
// staging ARE the bytes in prod" a checkable invariant — the digests are
// frozen the moment the env is promoted, independent of any later edit or move
// of the release file.
//
// EVERYTHING THAT CAN REFUSE THE PROMOTE IS CHECKED BEFORE THE WRITE. A flag
// this build does not support, a malformed gate, a bad run id: each refuses
// the whole command, rather than moving the pointer and then failing on the
// part the caller actually asked for.
func runPromote(ctx context.Context, version, env string, opts promoteOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validatePromoteFollow(opts.Follow); err != nil {
		return err
	}
	gates, err := resolvePromoteGates(opts.Gates)
	if err != nil {
		return err
	}
	source, err := resolvePromoteFrom(ctx, version, env, opts.ProjectDir, opts.From)
	if err != nil {
		return err
	}
	version = source.Version
	if version == "" {
		return fmt.Errorf("name the release to promote: forge env promote <version> --to %s", env)
	}
	run, err := opts.Run.resolveRun()
	if err != nil {
		return err
	}

	projectDir := opts.ProjectDir
	if projectDir == "" {
		projectDir = projectDirForKCL()
	}
	bindings, releases := opts.Bindings, opts.Releases
	if bindings == nil || releases == nil {
		l, err := ledgerFor(ctx, projectDir, env)
		if err != nil {
			return err
		}
		if bindings == nil {
			bindings = l.Bindings
		}
		if releases == nil {
			releases = l.Releases
		}
	}

	plan, err := computePromotePlan(ctx, promotePlanOptions{
		Env:        env,
		Version:    version,
		ProjectDir: projectDir,
		Bindings:   bindings,
		Releases:   releases,
		Git:        opts.Git,
	})
	if err != nil {
		return err
	}
	plan.DryRun = opts.DryRun
	plan.SourceNote = source.Note
	guard := guardFor(plan, opts.ExpectCurrent, opts.Supersede)
	if guard.ExpectUnbound {
		plan.Expected = expectUnboundLiteral
	} else {
		plan.Expected = guard.ExpectedCurrentID
	}

	// THE ONLY WRITE IN THIS COMMAND, and it is downstream of the plan. A
	// dry run simply skips it; everything rendered below is the same value
	// either way, which is why --plan cannot describe a different change
	// than the one that gets made.
	var writeErr error
	if !opts.DryRun {
		writeErr = applyPromotePlan(ctx, bindings, &plan, promoteWrite{
			By:    promoteActor(opts.Actor),
			Note:  opts.Note,
			Guard: guard,
			Gates: gates,
			Run:   run,
			// The resolved --from (F5, promote_from.go).
			FromEnv:           source.FromEnv,
			FromPromotionID:   source.FromPromotionID,
			VersionFromSource: source.VersionFromSource,
		})
		if writeErr == nil {
			writeErr = followPromote(ctx, env, plan, opts.Follow)
		}
	}
	// A refused write still renders: the plan is what the write WOULD have
	// done, and the refusal says what is there instead. An APPLIED promote
	// whose follow-through then failed renders too, and that case is the
	// one worth spelling out: a wait that degrades, times out or is
	// superseded (F3) arrives here in the same shape a failed write does,
	// but the two want opposite treatment. A failed write recorded
	// nothing, so there is no document worth reading beyond the error. A
	// failed WAIT recorded a promotion — `recorded.id` exists, and it is
	// precisely what the next pipeline step needs, because a red rollout
	// is exactly when the gate evidence must be attached to it. Dropping
	// the document there would leave CI's `gate record` with no promotion
	// id for the one release that needed the trail.
	//
	// plan.stamp(writeErr) below carries the follow-through's exit code
	// into ok/exit_code, so the document and the process status still
	// agree — the document reports applied:true with exit_code 5, which
	// is the truth: the pointer moved and the gate went red.
	if writeErr != nil && plan.Refusal == nil && !plan.Applied {
		return writeErr
	}

	plan.stamp(writeErr)
	if opts.JSON {
		if err := emitJSONDocument(plan); err != nil {
			return err
		}
		return writeErr
	}
	renderPromotePlanText(progressWriter(false), plan)
	return writeErr
}

// promoteActor is who the ledger entry names. An explicit --actor is an
// automation; otherwise the local user, best-effort. The hosted ledger
// ignores this for the human half and records the authenticated caller.
func promoteActor(actor string) release.Actor {
	if actor != "" {
		return release.Actor{Actor: actor}
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return release.Actor{User: u.Username}
	}
	return release.Actor{}
}
