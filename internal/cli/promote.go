package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// newPromoteCmd is `forge env promote <version> --to <env>`: bind an env to a
// release. This is the "promote, don't rebuild" half of the build-once model —
// a pure pointer move. It reads the release ledger `forge build --release`
// wrote, resolves each image's digest, and records env→release in the binding
// ledger (.forge/env-releases.json). No build runs; the bytes that were cut as
// <version> are, by construction, the bytes the env will deploy.
func newPromoteCmd() *cobra.Command {
	var (
		toEnv   string
		dryRun  bool
		jsonOut bool
	)

	cmd := &cobra.Command{
		Use:   "promote <version> --to <env>",
		Short: "Bind an environment to a release (build once, promote — no rebuild)",
		Long: `Bind an environment to an already-built release.

` + "`forge build --release <version>`" + ` builds the env-agnostic images ONCE,
captures their content-addressed digests, and writes a release ledger at
.forge/releases/<version>.json. ` + "`forge env promote`" + ` advances that release to
an environment BY REFERENCE: it records env → release (with the resolved
per-image digests snapshotted) in .forge/env-releases.json. No image is rebuilt
— the exact bytes cut as <version> are what the env ships.

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
  forge env promote v1.3.0 --to prod --plan | grep -i rollback   # catch a backwards move`,
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
			projectDir := projectDirForKCL()
			return runPromote(cmd.Context(), args[0], toEnv, promoteOptions{
				DryRun:     dryRun,
				JSON:       jsonOut,
				ProjectDir: projectDir,
				Releases:   readReleaseLedgers(projectDir),
			})
		},
	}

	cmd.Flags().StringVar(&toEnv, "to", "", "Environment to bind to the release (required)")
	cmd.Flags().BoolVar(&dryRun, "plan", false, "Compute and print the full change set WITHOUT writing the binding")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON (same exit codes as text mode)")

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
	Bindings   bindingStore
	Releases   []Release
	Git        promoteGitReader
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
	bindings := opts.Bindings
	if bindings == nil {
		bindings = bindingStoreFor(projectDir)
	}

	plan, err := computePromotePlan(ctx, promotePlanOptions{
		Env:        env,
		Version:    version,
		ProjectDir: projectDir,
		Bindings:   bindings,
		Releases:   opts.Releases,
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
		if err := applyPromotePlan(bindings, &plan); err != nil {
			return err
		}
	}

	if opts.JSON {
		return writePromotePlanJSON(plan)
	}
	renderPromotePlanText(plan)
	return nil
}
