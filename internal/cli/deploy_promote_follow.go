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
// FORGE DOES NOT APPLY TO A CLUSTER. The promotion this command records IS
// the deploy: it is declarative intent — "env X should run bundle B" — and a
// reconciler (Flux, from the bundle's manifest layer) converges the cluster to
// it. So the follow-through's job is no longer "apply, then wait"; it is
// "finish what forge still owns, then WAIT ON THE CONVERGENCE the reconciler
// performs".
//
// WHAT FORGE STILL OWNS, and why it is not nothing. An env is a set of
// GROUPS, not a mode, and only the cluster ones are converged from a bundle:
//
//	cluster objects   converged by the reconciler from the promoted bundle.
//	                  forge applies none of them, resolves no kubectl context,
//	                  and needs no kubeconfig.
//	hosted workloads  PUBLISHED to the control plane (specs it will run).
//	                  Publishing is not applying — there is no cluster at the
//	                  other end of it, and nothing else would do it.
//	compose, host
//	infra, shipped
//	frontends         still deployed from this machine. They are not
//	                  Kubernetes objects, no Kustomization carries them, and
//	                  if forge stopped nothing would deploy them at all.
//
// A MACHINE-LEDGER env (promotions in .forge/promotions/<env>.jsonl, no
// control plane) has no reconciler yet — nothing installs Flux into its
// cluster or writes the in-cluster OCIRepository pointer. Until F-FLUX-LOCAL
// does, it keeps the client-side apply, through exactly one function that
// names its own deletion: applyClusterPendingLocalFlux.
//
// THE PROPERTY A CALLER RELIES ON IS UNCHANGED: "the release is live or this
// command is red". What changed is who made it live.
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
	"os"
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

	// jsonOut mirrors the command's --json. The follow stage's progress
	// notices are for a HUMAN, so under --json they must go to stderr:
	// `--json` promises stdout carries exactly one JSON document, and a
	// notice printed beside it makes the document undecodable for every
	// consumer — which is how `--json --no-wait` came to emit
	// "Recorded. …" and then a perfectly good document that no caller
	// could parse.
	jsonOut bool
}

// notice writes a human progress line for the follow stage. It goes to stderr
// under --json (see promoteFollowOptions.jsonOut) and stdout otherwise, so the
// machine contract holds without the human losing the message.
func (o promoteFollowOptions) notice(format string, args ...any) {
	w := os.Stdout
	if o.jsonOut {
		w = os.Stderr
	}
	fmt.Fprintf(w, format, args...)
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

// followPromote finishes what forge still owns after the promotion is
// recorded, then gates on the convergence. plan.Recorded is the promotion the
// env now resolves to — the new entry, or the existing one for an idempotent
// retry.
//
// TWO SHAPES, decided by the LEDGER and nothing else:
//
//	CONTROL-PLANE ledger   forge applies NO cluster object. It deploys the
//	                       halves nothing converges (compose, host infra,
//	                       frontends), publishes any hosted workloads, and
//	                       waits on the control plane's convergence read.
//	MACHINE ledger         no reconciler exists for it yet, so the cluster
//	                       apply still runs from here, through the one named
//	                       transitional function. F-FLUX-LOCAL deletes it.
//
// The old third shape — MIXED, "apply the cluster half here and wait on the
// hosted half" — is gone, and that is the substance of this change. A mixed
// env's cluster objects are in the bundle like any other env's, so the
// reconciler converges them; what made mixed special was forge applying them,
// and it no longer does. The non-cluster halves it ALSO has are still deployed
// from here, which is why `reconcilerOwnsClusters` suppresses the clusters
// rather than the whole deploy.
//
// ORDER: the local work, then the wait. The local half is what this command
// can fail fast on, and spending the convergence budget after it has already
// failed buys nothing.
func followPromote(ctx context.Context, env string, plan promotePlan, ledger envLedger, o promoteFollowOptions) error {
	if !ledger.Hosted {
		// No control plane, so no reconciler: the client-side apply is
		// the only thing that will ever ship this env. Transitional —
		// see applyClusterPendingLocalFlux.
		return applyClusterPendingLocalFlux(ctx, env, plan, o)
	}

	// Everything the reconciler does NOT converge, from this machine: a
	// compose or host-infra workload, a shipped frontend, and the PUBLISH
	// of any hosted workload. Cluster objects are excluded inside
	// runDeploy, by reconcilerOwnsClusters.
	//
	// It runs for EVERY control-plane env, not only a mixed one. The pure
	// hosted env needs it for its publish — before O-15 that was a second
	// command's job, and with one command it has to happen here or it never
	// happens (hounders prod is that env). An env with neither a hosted
	// workload nor a local half dispatches no groups and the call is a
	// no-op, which is cheaper than deciding in advance whether to make it.
	if err := deployWhatTheReconcilerDoesNot(ctx, env, o); err != nil {
		return err
	}
	if o.NoWait {
		// The reconciler converges the promotion on its own. Saying so
		// is the difference between "forge is done" and "the release is
		// live", and a caller who opted out of the gate is exactly the
		// one who needs to know which they got.
		o.notice("\nRecorded. %s converges to it on its own; gate on it with: forge env status %s --wait\n", env, env)
		return nil
	}
	promotionID := ""
	if plan.Recorded != nil {
		promotionID = plan.Recorded.ID
	}
	// A hosted promotion the control plane cannot converge has nothing to
	// wait for, and waiting anyway is a 15-minute silence ending in UNKNOWN.
	// Refuse now, naming the one command that fixes it. See
	// refuseUnpublishedHostedDeploy.
	if err := refuseUnpublishedHostedDeploy(ctx, env, promotionID); err != nil {
		return err
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

// deployWhatTheReconcilerDoesNot is the local half of a control-plane env's
// deploy: everything in it that no Kustomization carries.
//
// It is the ORDINARY deploy path with the clusters taken out
// (reconcilerOwnsClusters), not a second, smaller publish. That matters for
// the same reason the old applySelfManaged reused it: a separate
// implementation would be a second opinion about what a deploy of this env
// means, and the two would diverge the first time either changed. So compose
// workloads, host infra, shipped frontends and the hosted PUBLISH all go
// through the one function, and the cluster groups are dropped from the
// dispatch set rather than from the code path.
//
// WHAT IT NEVER TOUCHES: a cluster. No kubectl context is resolved, no
// preflight runs against a live apiserver, no Secret is projected, no
// kubeconfig is minted — because forge may hold no credential for the cluster
// at all, and a deploy that failed on a kubeconfig it should never have needed
// is how "forge does not apply" becomes "forge cannot deploy".
//
// The deploy re-reads the digests from the env's own ledger rather than being
// handed them, and that is sound: it reads the promotion this same process
// just recorded, so it pins exactly the bytes that were promoted.
//
// --no-wait / --timeout / --fail-fast tune the rollout policy here as well as
// the convergence wait that follows. The policy governs real resources (a
// compose service, a one-shot Job), so it is a real wait; passing the flags to
// both is deliberate, so one --timeout does not silently come to mean "per
// half".
func deployWhatTheReconcilerDoesNot(ctx context.Context, env string, o promoteFollowOptions) error {
	opts := rolloutTunedDeploy(o)
	opts.reconcilerOwnsClusters = true
	o.notice("\nDeploying the parts of %s nothing converges (its cluster objects ride its promoted bundle)\n", env)
	// Under --json the promote owns the single document on stdout, and this
	// deploy is a phase inside it rather than a command of its own. It
	// prints with fmt.Printf throughout (group headers, per-deployment
	// lines), so os.Stdout is diverted for its duration — the same
	// mechanism runEnvRender uses, and for the same reason: one document on
	// stdout, the whole human log still readable on stderr.
	if o.jsonOut {
		real := os.Stdout
		os.Stdout = os.Stderr
		defer func() { os.Stdout = real }()
	}
	return runPromoteClientDeploy(ctx, env, opts)
}

// applyClusterPendingLocalFlux is THE ONE TRANSITIONAL CLIENT-SIDE CLUSTER
// APPLY, and F-FLUX-LOCAL DELETES IT.
//
// An env with no control plane has no reconciler: nothing installs Flux into
// its cluster and nothing writes the in-cluster OCIRepository that would point
// at its bundle. So if this command does not apply the promotion it just
// recorded, nothing ever will — a local k3d env would record promotions
// forever and never run any of them.
//
// WHEN F-FLUX-LOCAL LANDS, `forge cluster up` installs the pinned Flux into
// the local cluster and forge writes that pointer itself. At that moment this
// function has no reason to exist and must be deleted outright, not left as a
// fallback: a fallback apply beside a working reconciler is two writers for one
// cluster, which is the exact condition the unified model removes.
//
// IT IS DELIBERATELY ONE FUNCTION WITH ONE CALLER, and a guard test pins that
// (deploy_promote_follow_test.go). The hazard is not that it exists — it has
// to, today — but that something else grows a dependency on it, because then
// deleting it stops being a deletion and becomes a refactor nobody schedules.
//
// Its own rollout wait IS the health gate here: there is no reconciler to
// observe, so there is no convergence read to follow it with, and
// `forge env status --wait` says so rather than timing out. --no-wait and
// --timeout therefore tune THAT policy.
func applyClusterPendingLocalFlux(ctx context.Context, env string, _ promotePlan, o promoteFollowOptions) error {
	o.notice("\nApplying %s's newly recorded release from this machine "+
		"(it declares no control plane, so no reconciler converges it)\n", env)
	return runPromoteClientDeploy(ctx, env, rolloutTunedDeploy(o))
}

// rolloutTunedDeploy maps the follow-through's gate flags onto the deploy's
// rollout policy. One place, so the local half and the transitional apply
// cannot come to disagree about what --timeout means.
func rolloutTunedDeploy(o promoteFollowOptions) deployOptions {
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
	return opts
}

// runPromoteWait is the hosted health gate. A var for ONE reason: a test must
// be able to assert that the wait is scoped to the promotion id the write
// RETURNED rather than to the env's current one, and that fact is only
// observable in the options the follow-through hands over. Production is
// runEnvWait, unchanged.
var runPromoteWait = runEnvWait

// runPromoteClientDeploy is the deploy both of the above run. A var so a test
// can assert THAT it ran and with WHICH options — in particular whether
// reconcilerOwnsClusters was set, which is the whole of "forge did not apply
// to the cluster" and is only observable here. Production is runDeploy,
// unchanged.
var runPromoteClientDeploy = runDeploy
