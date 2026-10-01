package cli

// `promote --wait` / `--deploy`: follow a promote through to its rollout
// (control-plane docs/design/hosted-deploy-primitives.md §3.2).
//
// OWNED BY F3. F2 declares the flags and the two hooks in promote.go so that
// file is never edited again; this file fills them in.
//
// WHY PROMOTE NEEDS THESE AT ALL. A promote moves a pointer and ships
// nothing, which is exactly right as a primitive and exactly wrong as a CI
// step: a pipeline's "deploy to prod" job that only promotes reports success
// before any byte has moved, and the release's actual failure surfaces
// minutes later with nothing connecting the two. `--wait` closes that by
// running `forge env wait`'s gate against THE PROMOTION THIS COMMAND JUST
// WROTE.
//
// AGAINST THE ID IT WROTE, NOT "whatever is current". The distinction is the
// whole value. If the wait re-read the env's current promotion, a hotfix
// landing in the seconds after this promote would silently become the thing
// being waited on, and the pipeline would report its own release as healthy
// on the strength of somebody else's. plan.Recorded carries the id the ledger
// returned, so the wait is pinned to it and an overtaking promote is reported
// as SUPERSEDED (exit 6) rather than passing.
//
// `--deploy` IS THE BRIDGE, NOT A SECOND WAY TO DEPLOY. An environment whose
// control plane does not converge promotions server-side (§1.3) will never
// apply the pointer this promote moved, so a plain `--wait` there can only
// time out — and would blame the release for a missing converger. `--deploy`
// runs the ordinary client-side deploy of the promotion just written, which
// is what makes the CI template usable before the converger ships for an env.

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// promoteFollowOptions are the flags that make a promote do more than move
// the pointer: ship it (--deploy) and/or wait for the rollout (--wait).
type promoteFollowOptions struct {
	// Wait blocks until the promotion this promote wrote (or the existing
	// one, for a no-op) finishes rolling out, with `forge env wait`'s exit
	// codes.
	Wait bool
	// Deploy runs the client-side deploy of the promotion just written —
	// the bridge for an env that does not converge promotions server-side.
	Deploy bool
	// Timeout is the whole wait budget; zero is the verb's default.
	Timeout time.Duration
	// FailFast exits 1 on the first DEGRADED observation instead of
	// waiting out Timeout.
	FailFast bool
}

// validatePromoteFollow runs BEFORE the plan is computed or anything is
// written, so a nonsensical combination refuses the whole promote rather than
// moving the pointer and then failing on the part the caller asked for.
//
// Both rules reject a flag that would otherwise do NOTHING SILENTLY, which is
// the failure worth refusing: a pipeline that passed `--fail-fast` and got no
// gate at all would believe it had one.
func validatePromoteFollow(o promoteFollowOptions) error {
	if o.FailFast && !o.Wait {
		return errors.New("--fail-fast tunes the --wait gate and does nothing on its own: pass --wait, " +
			"or drop --fail-fast (a promote that does not wait has no rollout to fail fast on)")
	}
	if o.Timeout != 0 && !o.Wait {
		return errors.New("--timeout bounds the --wait gate and does nothing on its own: pass --wait, " +
			"or drop --timeout (a promote that does not wait has nothing to time out)")
	}
	if o.Timeout < 0 {
		return fmt.Errorf("--timeout must be positive, got %s", o.Timeout)
	}
	return nil
}

// followPromote runs after a successful write. plan.Recorded is the promotion
// the env now resolves to — the new entry, or the existing one for an
// idempotent retry.
//
// THE ORDER IS DEPLOY THEN WAIT, and it is the only order that works:
// --deploy exists precisely for envs where nothing applies the promotion on
// its own, so waiting first would wait for something that has not been asked
// to happen yet. A retried CI job is safe either way — the deploy is
// idempotent (it publishes the same frozen pins) and the wait is read-only.
func followPromote(ctx context.Context, env string, plan promotePlan, o promoteFollowOptions) error {
	if !o.Wait && !o.Deploy {
		return nil
	}
	promotionID := ""
	if plan.Recorded != nil {
		promotionID = plan.Recorded.ID
	}
	if o.Deploy {
		if err := deployPromotedRelease(ctx, env); err != nil {
			return err
		}
	}
	if !o.Wait {
		return nil
	}
	wait := envWaitOptions{
		PromotionID: promotionID,
		Timeout:     o.Timeout,
		FailFast:    o.FailFast,
		// --deploy has just applied the pins from this machine, so the
		// "this env does not converge promotions" fast refusal must
		// not fire: the thing it protects against — waiting out a
		// budget for a converger that was never going to run — cannot
		// happen when the client did the converging itself.
		AllowNonConverging: o.Deploy,
		// The promote's --json owns stdout, so the wait must not write
		// a second document to it. Its outcome rides out on the error,
		// which plan.stamp folds into the promote's envelope, so the
		// exit code is still the wait's.
		JSON: false,
	}
	return runPromoteWait(ctx, env, wait)
}

// runPromoteWait is the wait `promote --wait` performs. A var for ONE reason:
// a test must be able to assert that the wait is scoped to the promotion id
// the promote RETURNED rather than to the env's current one, and that fact is
// only observable in the options the follow-through hands over. Production
// is runEnvWait, unchanged.
var runPromoteWait = runEnvWait

// deployPromotedRelease is `--deploy`: the ordinary `forge env deploy <env>`
// of the promotion just written.
//
// It reuses the real deploy path rather than reimplementing a publish, which
// is what keeps "promote --deploy" and "promote then deploy" the same thing.
// The deploy resolves the digests from the ledger itself, and the ledger now
// holds the promotion this command just appended, so it pins exactly those
// bytes.
//
// The rollout policy is left at its default (wait-and-fail): a --deploy whose
// publish never became ready has not delivered the release, and reporting
// that as a successful promote is the gap --deploy exists to close.
func deployPromotedRelease(ctx context.Context, env string) error {
	fmt.Printf("\nDeploying %s's newly promoted release (--deploy)\n", env)
	return runDeploy(ctx, env, deployOptions{})
}
