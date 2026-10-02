package cli

// The RELEASE half of `forge env deploy <env> [vX | --from <src-env>]` (ADR
// docs/adr/env-verbs.md, task V3). Was promote.go, reached as
// `forge env deploy <env> <version>`.
//
// WHAT THIS HALF DOES, AND WHAT IT DELIBERATELY DOES NOT. It reads the release
// `forge env build --release` cut, freezes each image's digest, and APPENDS one
// entry to the env's promotion ledger — the project's
// .forge/promotions/<env>.jsonl, or the control plane the env's KCL declares.
// No build runs; the bytes that were cut as <version> are, by construction,
// the bytes the env will run. It ships NOTHING: the apply and the health gate
// are the other half of the verb, in deploy_promote_follow.go.
//
// Keeping the ledger write separate from the apply is what makes the invariant
// "the bytes that passed staging ARE the bytes in prod" checkable — the
// digests are frozen the moment the env is promoted, independent of any later
// edit or move of the release file, and independent of whether the apply
// succeeded.

import (
	"context"
	"fmt"
	"os/user"

	"github.com/reliant-labs/forge/pkg/release"
)

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

	// Follow is the apply-and-gate stage that runs after the ledger write
	// (deploy_promote_follow.go). NIL MEANS LEDGER-WRITE ONLY: nothing is
	// applied and nothing is waited on.
	//
	// `forge env deploy` always states it, so the verb always means
	// record + apply + wait. Nil is for the callers whose subject is the
	// ledger itself — the plan/CAS/gates/provenance tests, and
	// `forge release`'s own fixtures — which assert what was WRITTEN and
	// would otherwise need a cluster to do it.
	//
	// A POINTER AND NOT A BOOL BESIDE A STRUCT, because the struct's zero
	// value is meaningful and it is "wait": `&promoteFollowOptions{}` is a
	// real request for the default follow-through, and no bool
	// combination can express that as distinctly as the pointer does.
	Follow *promoteFollowOptions

	Gates []string
	From  promoteFromOptions
	// Run is the run identity; resolved against the CI environment.
	Run runOptions

	// Ledger is the env's release ledger: where the promotion is recorded
	// AND — through its Hosted field — who applies it.
	//
	// REQUIRED, and resolved by the CALLER. `forge env deploy` resolves it
	// from the env's declaration (resolveReleaseLedger) before calling;
	// a test states one directly. Leaving it nil for runPromote to resolve
	// would make the field one no production path ever sets, so every read
	// here would observe only a test's value — the shape
	// internal/deadcodeguard calls a phantom field, and it is right to:
	// the seam would be exercised exclusively by the tests that use it.
	//
	// ONE SEAM AND NOT THREE. An earlier shape had Bindings, Releases and a
	// *bool Hosted as separate fields, which let a caller state a hosted
	// store and leave Hosted false — describing an env whose promotions
	// live on a control plane that then applies nothing. envLedger already
	// holds the three together and cannot express that: the hosted
	// constructor sets Hosted:true beside the stores, so where a promotion
	// is recorded and who converges it are decided once, from one
	// declaration.
	Ledger envLedger
	Git    promoteGitReader

	// Confirm is the O-13 approval gate, run between the plan and the write
	// (deploy_confirm.go). NIL MEANS NO GATE, and only the callers whose
	// subject is the ledger itself pass nil — the plan/CAS/gates/provenance
	// tests and `forge release`'s fixtures, exactly the callers that also
	// pass a nil Follow. `forge env deploy` always states it, so the verb
	// always shows the plan and asks before writing.
	Confirm *deployConfirm
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
	if opts.Follow != nil {
		if err := validatePromoteFollow(*opts.Follow); err != nil {
			return err
		}
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
		return fmt.Errorf("name the release to deploy: forge env deploy %s <version>", env)
	}
	run, err := opts.Run.resolveRun()
	if err != nil {
		return err
	}

	projectDir := opts.ProjectDir
	if projectDir == "" {
		projectDir = projectDirForKCL()
	}
	ledger := opts.Ledger
	if ledger.Bindings == nil || ledger.Releases == nil {
		return fmt.Errorf("internal: no release ledger resolved for env %q "+
			"(the caller must pass promoteOptions.Ledger — see resolveReleaseLedger)", env)
	}

	plan, err := computePromotePlan(ctx, promotePlanOptions{
		Env:        env,
		Version:    version,
		ProjectDir: projectDir,
		Bindings:   ledger.Bindings,
		Releases:   ledger.Releases,
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

	// THE CONFIRMATION GATE (O-13). The plan is printed and approved before
	// anything is written — see deploy_confirm.go for why the review had to
	// move in front of the write rather than beside it.
	//
	// It runs for BOTH deploy forms, because both write a promotion and a
	// promotion is the deploy. --plan was already a write-nothing preview, so
	// it skips the gate: there is no write to gate.
	//
	// Nil Confirm means "no gate", and the only callers that pass nil are the
	// ones whose subject IS the ledger — the plan/CAS/gates/provenance tests
	// and `forge release`'s fixtures, the same callers that pass a nil Follow.
	// Production always states it.
	if !opts.DryRun && opts.Confirm != nil {
		plan.renderForConfirmation(opts.JSON)
		outcome := confirmDeployPlan(env, plan, *opts.Confirm)
		if outcome.Err != nil {
			return outcome.Err
		}
		if !outcome.Confirmed {
			// Declined, or --plan-only. Nothing was written, and that is a
			// SUCCESS: the caller asked what would happen and found out.
			plan.Confirmed = false
			plan.stamp(nil)
			if opts.JSON {
				return emitJSONDocument(plan)
			}
			return nil
		}
		plan.Confirmed = true
	}

	// THE ONLY WRITE IN THIS COMMAND, and it is downstream of the plan. A
	// dry run simply skips it; everything rendered below is the same value
	// either way, which is why --plan cannot describe a different change
	// than the one that gets made.
	var writeErr error
	if !opts.DryRun {
		writeErr = applyPromotePlan(ctx, ledger.Bindings, &plan, promoteWrite{
			By:    promoteActor(opts.Actor),
			Note:  opts.Note,
			Guard: guard,
			Gates: gates,
			Run:   run,
			// The resolved --from (promote_from.go).
			FromEnv:           source.FromEnv,
			FromPromotionID:   source.FromPromotionID,
			VersionFromSource: source.VersionFromSource,
		})
		if writeErr == nil && opts.Follow != nil {
			writeErr = followPromote(ctx, env, plan, ledger, *opts.Follow)
		}
	}
	// A refused write still renders: the plan is what the write WOULD have
	// done, and the refusal says what is there instead. An APPLIED promote
	// whose follow-through then failed renders too, and that case is the
	// one worth spelling out: a wait that degrades, times out or is
	// superseded arrives here in the same shape a failed write does,
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

// resolveReleaseLedger is how `forge env deploy` answers both halves of "where
// does this release live" before it starts: WHERE the promotion is recorded,
// and — through envLedger.Hosted — WHO applies it.
//
// It is the production writer of promoteOptions.Ledger, and it exists as its
// own function so that fact is visible at the command rather than buried as a
// nil-check inside runPromote. Resolving it there would have made the seam a
// field only tests ever set.
func resolveReleaseLedger(ctx context.Context, projectDir, env string) (envLedger, error) {
	if projectDir == "" {
		projectDir = projectDirForKCL()
	}
	return ledgerFor(ctx, projectDir, env)
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
