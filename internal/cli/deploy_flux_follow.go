package cli

// The two questions [followPromote] asks before handing a deploy to the
// reconciled path: IS this env reconciled, and WHICH BUNDLE does it point at.
//
// Both are deliberately small and both fail SOFT in one specific direction,
// which is worth stating because the direction is not symmetric.
//
// [fluxReconciledEnv] fails soft toward the EXISTING path: a render it cannot
// read means it answers false, and the deploy applies as it always did. That
// is right because the render has already succeeded once by the time a deploy
// reaches the follow-through, so a failure here is a transient or a changed
// checkout rather than a fact about the env — and answering true on a render
// forge could not read would write a pointer from a cluster list it does not
// have.
//
// [fluxDeployDigest] fails soft toward NO DIGEST, which the caller turns into
// a refusal naming `forge env build`. There is no safe fallback: a pointer
// needs immutable bytes, and the one thing that must not happen is a reconciled
// env silently applying from this machine because its bundle could not be
// found.

import (
	"context"

	"github.com/reliant-labs/forge/pkg/release"
)

// fluxReconciledEnv reports whether this env is converged by a Flux in its own
// cluster, and returns the render the pointer is built from.
//
// It renders rather than taking the entities as a parameter because
// followPromote does not have them: it is handed a promotion plan and a
// ledger, and every other branch it takes re-enters the deploy (which renders
// for itself). Rendering here keeps the decision beside the data it is made
// from — and the render is cached by the KCL layer, so this is not a second
// evaluation.
func fluxReconciledEnv(ctx context.Context, env string, ledger envLedger) (bool, *KCLEntities) {
	if ledger.Hosted {
		// An env with a control plane has a version store that already
		// drives a reconciler. Short-circuited before the render
		// because the answer cannot depend on it.
		return false, nil
	}
	entities, err := RenderKCL(ctx, projectDirForKCL(), env)
	if err != nil {
		// Soft, toward the existing path — see the file header.
		return false, nil
	}
	if !reconcilesThroughFlux() {
		return false, nil
	}
	return true, entities
}

// fluxDeployDigest is the bundle the pointer pins: the one recorded for the
// release this promotion just bound.
//
// IT READS THE LEDGER, NOT THE RENDER, and that is the whole point of the
// bundle model. The pointer must name the bytes the promotion recorded, so
// that "what is running" and "what was recorded" are the same claim. Deriving
// it from a fresh render would make the deployed artifact a function of the
// checkout at deploy time — a moved sibling repo, a different forge, a dirty
// tree — and the ledger's record would describe something else.
//
// The release comes from the promotion that was just WRITTEN
// (plan.Recorded), not from the plan's target, because those differ on an
// idempotent retry: a re-deploy of the env's current state returns the
// EXISTING promotion, and the digest to point at is that one's. Falling back
// to the plan's target covers the dry-run and nil-Recorded shapes, where no
// write happened and the target is all there is.
func fluxDeployDigest(ctx context.Context, env string, plan promotePlan) string {
	version := plan.Target.Release
	if plan.Recorded != nil && plan.Recorded.Release != "" {
		version = plan.Recorded.Release
	}
	digest, err := bundleDigestForRelease(ctx, projectDirForKCL(), env, version, envLedger{})
	if err != nil {
		return ""
	}
	if digest != "" {
		return digest
	}
	// An UNRELEASED bundle — a dev build, or a deploy of a tree nobody cut
	// a release from — is recorded with an empty Release, so the lookup by
	// version finds nothing. That is the ordinary shape for exactly the
	// envs this path serves (dev-k8s, e2e), so the newest bundle recorded
	// for the env is the answer rather than a refusal.
	//
	// NEWEST RECORDED, not newest applied: this runs immediately after the
	// promotion that pins it, so the newest bundle for this env IS the one
	// being deployed. An older one would point the cluster at a previous
	// release while the ledger recorded this one.
	return newestRecordedBundleDigest(projectDirForKCL(), env)
}

// newestRecordedBundleDigest is the most recent bundle recorded for an env,
// whatever release it pins.
func newestRecordedBundleDigest(projectDir, env string) string {
	store, err := recordStoreFor(projectDir)
	if err != nil {
		return ""
	}
	bundles, err := store.store.Bundles(env)
	if err != nil {
		return ""
	}
	// Oldest first, so walk backwards.
	for i := len(bundles) - 1; i >= 0; i-- {
		if release.ValidDigest(bundles[i].Digest) {
			return bundles[i].Digest
		}
	}
	return ""
}
