package cli

// The pre-wait guard for `forge env deploy <env> <version>` on a HOSTED env:
// refuse a promotion the control plane has nothing to converge, instead of
// waiting out the budget for a rollout that cannot start.
//
// ── The gap this closes ──────────────────────────────────────────────────────
//
// On a hosted env, V3's deploy records the promotion and then waits, because
// the control plane's converger is what applies it. That is correct — and it
// assumes the env's workloads have already been PUBLISHED, because the
// converger only ever patches the image digest on a hub OCIRepository that
// already exists. It creates nothing (control-plane
// internal/deployconverge/doc.go, hosted-deploy-primitives §9), so on an env
// whose tiers were never published there is no object to patch and no
// deployment row to converge.
//
// Publishing is the DECLARATION path: `forge env deploy <env>` with no
// version, which renders the env and calls EnsureDeployment per workload. So
// the FIRST deploy of a hosted environment is two commands, and naming a
// version on the first one skips the half that creates the workloads.
//
// ── Why this was expensive without the guard ─────────────────────────────────
//
// Measured on the k3d e2e stack: `forge env deploy <env> <version>` against a
// never-published hosted env wrote the promotion, then polled GetRollout every
// 5s for the full 15m budget. The control plane answered `status=ok` each
// time — an env with no deployments has no unhealthy workload, so there is
// nothing for it to report as wrong — and the deploy ended with
// "Rollout … UNKNOWN", followed by advice to run the very deploy it had just
// run. Nothing in that output names the missing publish, and the ledger says
// the promotion was recorded, so the env reads as deployed while running
// nothing at all.
//
// A rollout with ZERO pinned workloads is the exact signature, and it is not
// ambiguous: a promotion pins artifacts, so a promotion that pins nothing
// cannot be a release anybody is waiting on.

import (
	"context"
	"fmt"
)

// refuseUnpublishedHostedDeploy returns a non-nil error when the env's hosted
// rollout names no pinned workload, which means the env's tiers were never
// published and no converger will ever move them.
//
// IT ONLY APPLIES TO AN ENV THAT DECLARES HOSTED WORKLOADS, and that scoping
// is load-bearing now that forge does not apply. "The rollout names no
// workload" used to be unambiguous, because a control-plane env's reason to
// exist was its hosted tiers. It is not any more: a CLUSTER-ONLY env on a
// control plane — the shape every env takes once its clusters are reconciled
// from their bundle — has no hosted workload by construction, so its rollout
// legitimately names none and this guard would refuse every deploy of it,
// advising a publish of workloads it does not have.
//
// So the question is asked of the DECLARATION first: only an env that should
// have published something can have failed to.
//
// Neither read is fatal. This guard exists to replace a silent 15-minute wait
// with one sentence; if the pre-check cannot render the env or read the
// rollout, the right outcome is to let the real wait run and report its own
// verdict, not to invent a refusal from a transport error.
func refuseUnpublishedHostedDeploy(ctx context.Context, env, promotionID string) error {
	entities, err := RenderKCL(ctx, projectDirForKCL(), env)
	if err != nil || entities == nil || !entities.HasHosted() {
		return nil // nothing was supposed to be published — see the docstring
	}
	target, err := resolveDeclaredWaitTarget(ctx, env)
	if err != nil {
		return nil // not our verdict to give
	}
	rollout, err := readRollout(ctx, target.Client, target.EnvironmentID, promotionID)
	if err != nil {
		return nil
	}
	return unpublishedHostedRefusal(env, rollout)
}

// unpublishedHostedRefusal is the DECISION, split from the read so it can be
// tested over rollout shapes without a control plane. Returns nil unless the
// rollout names no workload at all.
func unpublishedHostedRefusal(env string, rollout wireRollout) error {
	// Unpinned rows count as published. A database is not release-bound and
	// never gates a release (owner ruling Q4), but its presence proves the
	// env's tiers WERE published — which is the only thing this guard asks.
	if len(rollout.Workloads) > 0 || len(rollout.Unpinned) > 0 {
		return nil
	}
	return &exitCodeError{code: exitUndetermined, msg: fmt.Sprintf(
		"%s has no published workloads, so its control plane has nothing to converge and this "+
			"release would never roll out.\n"+
			"  The promotion IS recorded — nothing was lost — but a hosted env's workloads are "+
			"created by the DECLARATION deploy, and the converger only moves the digest on "+
			"workloads that already exist.\n"+
			"  Publish them first, then this release is already bound and will converge:\n"+
			"      forge env deploy %s\n"+
			"      forge env status %s --wait", env, env, env)}
}
