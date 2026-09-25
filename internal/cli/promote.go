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
// It reads the release `forge build --release` cut, freezes each image's
// digest, and APPENDS one entry to the env's promotion ledger — the project's
// .forge/promotions/<env>.jsonl, or the control plane the env's KCL declares.
// No build runs; the bytes that were cut as <version> are, by construction,
// the bytes the env will deploy.
func newPromoteCmd() *cobra.Command {
	var (
		toEnv    string
		dryRun   bool
		jsonOut  bool
		rollback bool
		note     string
		actor    string
	)

	cmd := &cobra.Command{
		Use:   "promote <version> --to <env>",
		Short: "Bind an environment to a release (build once, promote — no rebuild)",
		Long: `Bind an environment to an already-built release.

` + "`forge build --release <version>`" + ` builds the env-agnostic images ONCE,
captures their content-addressed digests, and cuts a release. ` + "`forge env promote`" + `
advances that release to an environment BY REFERENCE: it appends one entry —
env, release, and the per-image digests frozen at this moment — to the env's
append-only promotion ledger. No image is rebuilt — the exact bytes cut as
<version> are what the env ships.

WHERE THE LEDGER LIVES is declared by the environment, not chosen by a flag:
an env whose KCL declares forge.ControlPlane records promotions on that control
plane; every other env records them in .forge/promotions/<env>.jsonl.

ROLLBACK IS A NEW ENTRY, NOT AN EDIT. --rollback records the entry as a
rollback, and the ledger refuses it unless the env has run that release
before — rolling "back" to something that never ran is a promotion, and must
be recorded as one. Re-promoting the release an env already runs appends
nothing; a CI retry is safe.

` + "`forge env deploy <env>`" + ` then pins those SAME digests, so every env promoted
to the same release deploys byte-identical images. This eliminates the per-env
rebuild that re-cross-compiles (and can drift arch/tag) for every environment.

SEE THE CHANGE BEFORE IT IS WRITTEN. --plan computes the ENTIRE change set and
writes nothing: the release the env runs now versus the one it would move to,
every image classified as unchanged / changed / added / removed (with both
digests where they differ), the git commits between the two releases, and —
the fact most worth reading twice — the DIRECTION. A promote to an older
release is a legitimate rollback, and it is reported as one rather than left
for you to infer from version numbers.

The plan and the real promote are computed by the SAME function, so the
preview cannot disagree with the write. --json emits it machine-readably, in
one document shape for both modes, with an ` + "`applied`" + ` field saying which one you
got.

PROMOTE SHIPS NOTHING. It moves a pointer. No image reaches any cluster until
` + "`forge env deploy <env>`" + ` runs, and ` + "`forge env verify <env>`" + ` is how you prove it
arrived. The plan says so on every invocation.

Examples:
  forge build --release v1.4.0 --push ghcr.io/acme   # build once, cut the release
  forge env promote v1.4.0 --to staging --plan            # what WOULD change (writes nothing)
  forge env promote v1.4.0 --to staging --plan --json     # the same, machine-readable
  forge env promote v1.4.0 --to staging                  # bind staging → v1.4.0
  forge env deploy staging                               # ships v1.4.0's digests
  forge env promote v1.4.0 --to prod                     # same digests advance to prod
  forge env deploy prod                                  # the bytes that passed staging
  forge env promote v1.3.0 --to prod --plan | grep -i rollback   # catch a backwards move
  forge env promote v1.3.0 --to prod --rollback --note "5xx spike" # record a rollback`,
		Args: cobra.ExactArgs(1),
		// The change set IS the output; a cobra usage dump would bury it
		// under the flag list.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if toEnv == "" {
				return fmt.Errorf("--to <env> is required: name the environment to bind to release %q", args[0])
			}
			// The checkout and its releases are resolved HERE, once, and
			// passed down. runPromote and computePromotePlan both still
			// fall back when these are empty — that is what lets a test
			// state them — but the production path states them too, so
			// the fields carry a real value rather than only ever the
			// zero one a test overwrites.
			kind := release.KindPromote
			if rollback {
				kind = release.KindRollback
			}
			return runPromote(cmd.Context(), args[0], toEnv, promoteOptions{
				DryRun:     dryRun,
				JSON:       jsonOut,
				ProjectDir: projectDirForKCL(),
				Kind:       kind,
				Note:       note,
				Actor:      actor,
			})
		},
	}

	cmd.Flags().StringVar(&toEnv, "to", "", "Environment to bind to the release (required)")
	cmd.Flags().BoolVar(&dryRun, "plan", false, "Compute and print the full change set WITHOUT writing the binding")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON (same exit codes as text mode)")
	cmd.Flags().BoolVar(&rollback, "rollback", false, "Record this as a ROLLBACK (the env must have run the release before)")
	cmd.Flags().StringVar(&note, "note", "", "Why — recorded on the ledger entry (most valuable on a rollback)")
	cmd.Flags().StringVar(&actor, "actor", "", "Name the automation recording this (e.g. ci); default is the local user")

	return cmd
}

// promoteOptions carries the flags and the seams into runPromote.
//
// The three seams (Bindings, Releases, Git) are injected for the same reason
// env verify injects its three: a test asserting that --plan writes nothing,
// or that a rollback is reported as one, should be able to STATE the ledger
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
	// Kind is promote (default) or rollback.
	Kind release.PromotionKind
	// Note and Actor are recorded on the ledger entry.
	Note  string
	Actor string
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
func runPromote(ctx context.Context, version, env string, opts promoteOptions) error {
	if ctx == nil {
		ctx = context.Background()
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
		Kind:       opts.Kind,
		ProjectDir: projectDir,
		Bindings:   bindings,
		Releases:   releases,
		Git:        opts.Git,
	})
	if err != nil {
		return err
	}
	plan.DryRun = opts.DryRun

	// THE ONLY WRITE IN THIS COMMAND, and it is downstream of the plan. A
	// dry run simply skips it; everything rendered below is the same value
	// either way, which is why --plan cannot describe a different change
	// than the one that gets made.
	if !opts.DryRun {
		if err := applyPromotePlan(ctx, bindings, &plan, promoteActor(opts.Actor), opts.Note); err != nil {
			return err
		}
	}

	if opts.JSON {
		return writePromotePlanJSON(plan)
	}
	renderPromotePlanText(plan)
	return nil
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
