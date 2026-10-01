package cli

// The APPLY and the WAIT halves of `forge env deploy <env> vX` (ADR
// docs/adr/env-verbs.md, task V3). Was promote_wait.go, where the same
// machinery was reached by `promote --wait` / `--deploy`.
//
// WHY THESE ARE NO LONGER FLAGS. A promote moves a pointer and ships nothing,
// which is exactly right as a primitive and exactly wrong as the thing a
// person or a pipeline asks for. "Deploy prod to v1.4.0" that only recorded a
// binding reported success before any byte moved, and the release's actual
// failure surfaced minutes later with nothing connecting the two. Every
// pipeline therefore spelled it `promote --deploy --wait`, and the spellings
// that omitted either half were bugs waiting for an incident. So `deploy`
// means record + apply + wait, and the only flag left is the one that opts
// OUT (--no-wait).
//
// WHO APPLIES IS DECIDED BY THE LEDGER, NOT A FLAG. That is the whole of the
// hosted/self-managed parity the ADR asks for:
//
//   - A HOSTED env (its KCL declares forge.ControlPlane, so its promotions
//     live on that control plane) is converged server-side. forge applies
//     nothing; it waits on the rollout the control plane computes.
//   - A SELF-MANAGED env (promotions in .forge/promotions/<env>.jsonl) has
//     nothing watching its ledger, so this same command performs the ordinary
//     client-side render+apply against the binding it just wrote. There, the
//     apply's own per-resource rollout wait IS the health wait — there is no
//     server-computed rollout to poll, and `forge env status --wait` says so
//     rather than timing out.
//
// Both reach "the release is live or this command is red", which is the
// property a caller can rely on. Which machinery got there is an
// implementation detail of where the env is run.
//
// THE HOSTED WAIT IS PINNED TO THE PROMOTION THIS COMMAND WROTE, not to
// "whatever is current". The distinction is the whole value. If the wait
// re-read the env's current promotion, a hotfix landing in the seconds after
// this deploy would silently become the thing being waited on, and the
// pipeline would report its own release as healthy on the strength of
// somebody else's. plan.Recorded carries the id the ledger returned, so the
// wait is pinned to it and an overtaking promote is reported as SUPERSEDED
// (exit 6) rather than passing.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/reliant-labs/forge/internal/cluster"
)

// promoteFollowOptions is what happens AFTER the ledger write: the apply, and
// the health gate.
//
// THE ZERO VALUE WAITS. That is the point of V3, and it is why there is a
// NoWait field rather than a Wait one: a caller that forgets to ask for the
// gate gets it anyway, and the only way to end up without one is to say so.
type promoteFollowOptions struct {
	// NoWait skips the health gate. The ledger write and the apply still
	// happen; only the "and it became healthy" half is dropped.
	//
	// It exists for the pipeline that genuinely wants fire-and-forget —
	// a fan-out to nine environments where one job waits on all of them
	// afterwards — and for an operator who will watch
	// `forge env status <env> --wait` themselves.
	NoWait bool
	// Timeout is the whole wait budget; zero is the verb's default.
	Timeout time.Duration
	// FailFast exits 1 on the first DEGRADED observation instead of
	// waiting out Timeout.
	FailFast bool

	// clientDeploy is the deployOptions a SELF-MANAGED env's client-side
	// apply runs with — the flags `forge env deploy` already had
	// (--dry-run, --target, --namespace …), so a promote-and-apply and a
	// spec-change apply of the same env do the same thing.
	//
	// Unexported because it is assembled by the command, never by a
	// caller choosing a follow-through; a test states the two exported
	// fields and leaves this zero.
	clientDeploy deployOptions
}

// validatePromoteFollow runs BEFORE the plan is computed or anything is
// written, so a nonsensical combination refuses the whole command rather than
// moving the pointer and then failing on the part the caller asked for.
//
// Both rules reject a flag that would otherwise do NOTHING SILENTLY, which is
// the failure worth refusing: a pipeline that passed --fail-fast and got no
// gate at all would believe it had one.
func validatePromoteFollow(o promoteFollowOptions) error {
	if o.NoWait && o.FailFast {
		return errors.New("--fail-fast tunes the health gate and --no-wait removes it: pass one. " +
			"A deploy that does not wait has no rollout to fail fast on")
	}
	if o.NoWait && o.Timeout != 0 {
		return errors.New("--timeout bounds the health gate and --no-wait removes it: pass one. " +
			"A deploy that does not wait has nothing to time out")
	}
	if o.Timeout < 0 {
		return fmt.Errorf("--timeout must be positive, got %s", o.Timeout)
	}
	return nil
}

// followPromote applies the promotion the write just recorded and gates on its
// health. plan.Recorded is the promotion the env now resolves to — the new
// entry, or the existing one for an idempotent retry.
//
// hosted says which half runs, and it comes from the env's LEDGER (the single
// declarative discriminator forge already has: envLedger.Hosted). The two
// branches are exclusive on purpose. A self-managed env has no server-computed
// rollout, so polling one could only time out; a hosted env is converged by
// the control plane, so a client-side apply would be forge racing the
// converger to write the same specs.
func followPromote(ctx context.Context, env string, plan promotePlan, hosted bool, o promoteFollowOptions) error {
	if !hosted {
		return applySelfManaged(ctx, env, o)
	}
	if o.NoWait {
		// The control plane converges the binding on its own. Saying so
		// is the difference between "forge is done" and "the release is
		// live", and a caller who opted out of the gate is exactly the
		// one who needs to know which they got.
		fmt.Printf("\nRecorded. %s is converged by its control plane; gate on it with: forge env status %s --wait\n", env, env)
		return nil
	}
	promotionID := ""
	if plan.Recorded != nil {
		promotionID = plan.Recorded.ID
	}
	return runPromoteWait(ctx, env, envWaitOptions{
		PromotionID: promotionID,
		Timeout:     o.Timeout,
		FailFast:    o.FailFast,
		// The deploy's --json owns stdout, so the wait must not write a
		// second document to it. Its outcome rides out on the error,
		// which plan.stamp folds into the deploy's envelope, so the exit
		// code is still the wait's.
		JSON: false,
	})
}

// applySelfManaged is the client-side half: render the binding this command
// just wrote and apply it from this machine.
//
// It reuses the real deploy path rather than reimplementing a publish, which
// is what keeps "deploy prod v1.4.0" and "promote then deploy" the same thing.
//
// The deploy re-reads the digests from the env's own ledger rather than being
// handed them, and that is sound: it re-reads the jsonl line this same process
// appended a moment ago, so the newest entry is the promotion this command
// wrote and the apply pins exactly the bytes that were just promoted.
//
// THE APPLY'S OWN ROLLOUT WAIT IS THE HEALTH GATE. A self-managed env has no
// server-computed rollout to poll, so there is no second gate to run after
// this one — cluster.RolloutPolicy already waits for every Deployment and
// one-shot Job and fails the deploy if any does not become ready. --no-wait
// and --timeout therefore tune THAT policy, which is why they are mapped onto
// it here instead of being passed to a wait that would have nothing to read.
func applySelfManaged(ctx context.Context, env string, o promoteFollowOptions) error {
	opts := o.clientDeploy
	if o.NoWait {
		opts.rollout.Mode = cluster.RolloutSkip
	}
	if o.Timeout > 0 {
		opts.rollout.Timeout = o.Timeout
	}
	if o.FailFast {
		opts.rollout.FailFast = true
	}
	fmt.Printf("\nApplying %s's newly recorded release (self-managed: no control plane converges it)\n", env)
	return runPromoteClientDeploy(ctx, env, opts)
}

// runPromoteWait is the hosted health gate. A var for ONE reason: a test must
// be able to assert that the wait is scoped to the promotion id the write
// RETURNED rather than to the env's current one, and that fact is only
// observable in the options the follow-through hands over. Production is
// runEnvWait, unchanged.
var runPromoteWait = runEnvWait

// runPromoteClientDeploy is the self-managed apply. A var for the same reason:
// a test asserts THAT the client-side apply ran (and with which options) for a
// file-ledger env, which is only observable here — the real apply needs a
// cluster. Production is runDeploy, unchanged.
var runPromoteClientDeploy = runDeploy
