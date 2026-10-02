package cli

// `forge env deploy <env>` with NO version: the one command that ships the
// checkout (owner decision O-15).
//
// THE OWNER'S WORDS: "the whole point is that deploy should do all the
// deployment bits, including building and pushing the release (OCI configs,
// the actual docker images, etc)." What prompted it was hitting this on
// hounders prod, from the UI:
//
//	hosted env "prod" has no promoted release, so there is no digest to
//	deploy … fix: forge env build prod --release <version> --no-build
//	&& forge env deploy prod <version>
//
// Three commands, one of which (--no-build) cuts a release over digests that
// do not exist, to do the thing the operator already asked for.
//
// So, in order:
//
//  1. BUILD at the current checkout, by exactly `forge env build`'s path —
//     images, static artifacts, and F-DECL's shape recording — and PUSH.
//  2. CUT a release under the auto-version rule, reusing one whose provenance
//     tree matches this checkout (deploy_autoversion.go), so a retried deploy
//     never cuts twice.
//  3. PLAN with the existing promote-plan machinery, and print it.
//  4. CONFIRM (deploy_confirm.go) — nothing is written until somebody says yes.
//  5. PROMOTE, apply and wait, by the same path `forge env deploy <env> <v>`
//     takes.
//
// WHAT THIS REPLACED, AND WHY THAT IS RIGHT. A no-version deploy used to be a
// "spec-change deploy": re-apply the env's CURRENT binding, for when the KCL
// moved and the release did not. That behaviour is NOT lost — it is what
// `forge env deploy <env> <current-version>` does, which is how §8.5's "Apply
// config" button spells it. What is gone is the spelling, because it made the
// most obvious invocation of the verb the one that could not ship the code in
// front of you. Pre-1.0: the old path is deleted, not kept beside the new one.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/pkg/release"
)

// runDeployEverything is the no-version form.
func runDeployEverything(ctx context.Context, envName string, f deployCmdFlags) error {
	projectDir := projectDirForKCL()
	rollout := cluster.RolloutPolicy{
		Mode:     cluster.RolloutMode(f.rolloutMode),
		Timeout:  f.rolloutTimeout,
		FailFast: f.rolloutFailFast,
		Order:    f.rolloutOrder,
	}
	if err := rollout.Validate(); err != nil {
		return err
	}

	// The ledger FIRST, before anything is built: it decides where the
	// release is cut and who applies the promotion, and an env whose
	// declaration cannot be resolved must fail before it burns a build.
	ledger, err := resolveReleaseLedger(ctx, projectDir, envName)
	if err != nil {
		return err
	}

	// Step 1 and 2. The version is decided from the checkout's provenance
	// BEFORE the build, so the build can push under it and the cut can
	// record what the push produced.
	//
	// UNDER --json THE BUILD'S OUTPUT GOES TO STDERR. `--json` promises
	// stdout carries exactly ONE document, and the build phase is a few
	// hundred lines of `[build] …` written with fmt.Printf — which would
	// land above the document and make it unparseable. Diverting
	// os.Stdout for the duration is the same mechanism runEnvRender uses
	// for the same reason; the build log stays fully readable on stderr.
	cut, err := func() (deployCutResult, error) {
		if f.jsonOut {
			real := os.Stdout
			os.Stdout = os.Stderr
			defer func() { os.Stdout = real }()
			return buildAndCutForDeploy(ctx, projectDir, envName, f, ledger)
		}
		return buildAndCutForDeploy(ctx, projectDir, envName, f, ledger)
	}()
	if err != nil {
		return err
	}

	// Steps 3-5 are the versioned deploy, verbatim: plan, confirm, promote,
	// apply, wait. That is the point — the no-version form decides WHICH
	// bytes and then hands over, so the two forms cannot diverge in how they
	// plan, confirm, compare-and-set, publish or gate.
	//
	// The apply rides the follow-through, INCLUDING a pure hosted env's
	// client-side publish, which followPromote now performs
	// (applyHostedPublish). Before O-15 that publish was reachable only from
	// the no-version form, so shipping a hosted env took two commands —
	// `deploy <env> <v> --no-wait` to record, then `deploy <env>` to
	// publish. Moving it into the follow-through is what lets ONE command do
	// it, and keeps the versioned form able to do it too rather than
	// stranding the publish on a spelling this change removes.
	p := f.promote
	p.version = cut.Version
	return runPromote(ctx, cut.Version, envName, promoteOptions{
		Ledger:        ledger,
		JSON:          f.jsonOut,
		ProjectDir:    projectDir,
		Note:          p.note,
		Actor:         p.actor,
		ExpectCurrent: p.expectCurrent,
		Supersede:     p.supersede,
		Gates:         p.gates,
		Run:           p.run,
		// AutoVersion is stated so the gate's refusal and its --plan-only
		// line can say "release X was cut and pushed; approving it needs no
		// rebuild" — without it a CI author reads "pass --yes" and re-runs
		// the no-version form, paying for the build twice.
		Confirm: newDeployConfirm(p, cut.Version),
		Follow: &promoteFollowOptions{
			NoWait:   p.noWait,
			jsonOut:  f.jsonOut,
			Timeout:  p.timeout,
			FailFast: p.failFast,
			clientDeploy: deployOptions{
				imageTag:      f.tag,
				namespace:     f.namespace,
				targetArch:    f.targetArch,
				prune:         f.prune,
				targets:       f.targets,
				skipFrontend:  f.skipFrontend,
				frontendsOnly: f.frontendsOnly,
				skipPreflight: f.skipPreflight,
				noDigest:      f.noDigest,
				rollout:       rollout,
			},
		},
	})
}

// deployCutResult is the release a no-version deploy will promote.
type deployCutResult struct {
	Version string
	// Reused is true when an existing release's provenance tree matched
	// this checkout, so nothing was cut.
	Reused bool
}

// buildAndCutForDeploy runs the build-and-cut half: resolve the provenance,
// reuse or name a version, build and push by `forge env build`'s own path,
// then cut.
//
// THE REUSE LOOKUP HAPPENS BEFORE THE BUILD, and the build still runs. Those
// are two different questions: the lookup decides whether a new VERSION is
// needed, while the build decides whether the images are present and pushed.
// Skipping the build on a reuse would be wrong — the registry may have
// expired the images under a retention window, the local state may be from a
// different checkout — and the build is content-addressed anyway, so a
// re-push of identical bytes is a no-op that resolves the same digests.
func buildAndCutForDeploy(ctx context.Context, projectDir, envName string, f deployCmdFlags, ledger envLedger) (deployCutResult, error) {
	prov := deployProvenance(ctx, projectDir)

	reusable, lookupErr := reusableReleaseForTree(ctx, ledger.Releases, prov.Tree)
	if lookupErr != nil {
		// Not fatal: "we could not look" means "cut a new one", and
		// failing a deploy because a list call hiccuped would turn an
		// optimisation into an outage.
		fmt.Printf("[deploy] Note: could not check for an existing release of this tree (%v); cutting a new version\n", lookupErr)
	}

	version := autoVersionFor(prov, time.Now())
	reused := false
	if reusable != nil {
		version = reusable.Version
		reused = true
		printAutoVersionReuse(*reusable)
	}

	// The BUILD, by `forge env build <env> --release <version>`'s own path:
	// --release implies --push and implies a docker build, and runBuild is
	// what records F-DECL's declaration, builds every artifact the env
	// declares, pushes each to its own declared reference, and cuts.
	opts := buildOptions{
		env:         envName,
		outputDir:   "bin",
		buildTarget: "all",
		parallel:    true,
		buildDocker: true,
		// pushIfDeclared, NOT push. Both push every image whose workload
		// declares a pushable reference; they differ on an env that declares
		// NONE, and that difference decides whether this verb works at all
		// for a whole class of project.
		//
		// --push treats "nothing to push" as a usage error, which is right
		// for `forge env build --push`: the author asked to publish and
		// there is nothing to publish. It is wrong here. A hosted env whose
		// images CI pushes — a third-party image, a workload built in
		// another pipeline — declares no reference forge builds, and such an
		// env is perfectly deployable: its release records the declared
		// images' digests (harvestHostedBackendArtifacts) and the deploy
		// pins them. Refusing it would mean `forge env deploy <env>` could
		// not ship an env that `forge env deploy <env> <v>` ships fine.
		//
		// So: push what is declared, and let an env with nothing to push
		// proceed to the cut. This is the mode `forge env up` already uses,
		// for the same reason.
		pushIfDeclared: true,
		release:        version,
		targetArch:     f.targetArch,
		targets:        f.targets,
		run:            f.promote.run,
	}
	if reused {
		// A reused version must not be RE-CUT with a different artifact
		// set: the ledger would refuse it as a conflict, which is correct
		// but reads as a failure of the deploy rather than of the re-cut.
		// The build still runs and still pushes; only the cut is skipped.
		opts.release = ""
	}
	if err := runDeployBuild(ctx, opts); err != nil {
		return deployCutResult{}, fmt.Errorf("build env %s for deploy: %w", envName, err)
	}
	if reused {
		return deployCutResult{Version: version, Reused: true}, nil
	}
	return deployCutResult{Version: version}, nil
}

// deployProvenance is captureBuildProvenance, as a var for the same reason
// runDeployBuild is one: a test's t.TempDir() is not a git repository, so the
// real capture reports no tree — and "no tree" is F-16's never-reuse path,
// which is the opposite of what a reuse test needs to exercise.
var deployProvenance = captureBuildProvenance

// runDeployBuild is runBuild, as a var so a test can state the build's
// outcome instead of needing a toolchain, a registry and a docker daemon.
//
// A SEAM AND NOT A MOCKED INTERFACE, because the thing under test is the
// ORDER — build, then cut, then plan, then confirm, then promote — and the
// order is observable from what the seam records. A test that replaced the
// whole build-and-cut half would be asserting its own structure.
var runDeployBuild = runBuild

// errUnknownReleaseForDeploy is `forge env deploy <env> <v>` naming a version
// nobody cut.
//
// THE FIX IS NEVER --no-build. That is what the owner hit: `forge env build
// <env> --release <v> --no-build` cuts a release over digests an earlier push
// left in .forge/state, and a version that was never cut has none — so the
// advice produced a release with no images in it, or failed its own
// completeness gate. A version that does not exist cannot be conjured from
// state that does not exist either. The honest remedy is the verb that builds
// it: `forge env deploy <env>`.
func errUnknownReleaseForDeploy(envName, version string, known []release.Release) error {
	msg := fmt.Sprintf("env %q: release %s was never cut, so there is nothing to deploy.\n", envName, version)
	if len(known) > 0 {
		msg += "  Releases this project has: " + releaseVersionList(known) + "\n"
	}
	msg += fmt.Sprintf("  fix: ship the current checkout — forge env deploy %s\n"+
		"       (builds, pushes, cuts a release named for this tree, then plans and deploys it)\n"+
		"  or name one of the releases above: forge env deploy %s <version>", envName, envName)
	return exitCodeError{code: exitWrong, msg: msg}
}

// releaseVersionList is the newest few versions, for the error above. Bounded
// because a project with hundreds of releases should not print all of them
// into a message whose subject is one typo.
func releaseVersionList(known []release.Release) string {
	const max = 5
	out := ""
	for i, rel := range known {
		if i == max {
			out += fmt.Sprintf(", … (%d more)", len(known)-max)
			break
		}
		if i > 0 {
			out += ", "
		}
		out += rel.Version
	}
	return out
}
