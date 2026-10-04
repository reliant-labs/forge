package cli

// THE LIVE SIDE OF A PLAN, for an env Flux converges.
//
// A machine-ledger env that reconciles through Flux never has forge apply it,
// so forge records no succeeded Apply and `appliedBundleShape` finds nothing:
// every plan read `live: none` and could not see a removal, including the
// removal of a PVC. Live is OBSERVED here instead, never written by forge's
// apply: it is the recorded bundle whose manifest digest equals the
// Kustomizations' `status.lastAppliedRevision`.
//
// UNKNOWN IS AN ANSWER. When the Kustomizations of a multi-cluster env report
// different revisions, when none has applied anything, when the cluster cannot
// be read, or when the revision names a bundle this machine never recorded,
// Live is unknown and the plan says so — guessing a side would manufacture
// removals (or hide them).

import (
	"context"
	"fmt"
	"time"

	"github.com/reliant-labs/forge/internal/flux"
	"github.com/reliant-labs/forge/pkg/release"
)

// fluxAppliedRevision reads the digest every Kustomization of the env agrees
// it last applied. ok is false, with a reason, when there is no single answer.
func fluxAppliedRevision(ctx context.Context, env string, entities *KCLEntities, doc release.BundleDoc) (digest, reason string, ok bool) {
	clusters := fluxTargetClusters(entities, doc.ClusterPaths)
	if len(clusters) == 0 {
		return "", "the bundle routes no documents to any cluster the env declares", false
	}
	pointers := make([]flux.Pointer, 0, len(clusters))
	for _, kctx := range clusters {
		pointers = append(pointers, flux.Pointer{Kustomizations: []flux.Object{
			{Kind: "Kustomization", Name: flux.KustomizationName(env, kctx), Namespace: flux.Namespace, Cluster: kctx},
		}})
	}
	obs, err := fluxObserve(ctx, pointers, time.Now())
	if err != nil {
		return "", fmt.Sprintf("the in-cluster reconciler could not be read (%v)", err), false
	}
	for _, s := range obs.Statuses {
		switch {
		case !s.Found:
			return "", fmt.Sprintf("Kustomization %s in %s does not exist", s.Name, s.Cluster), false
		case s.Revision == "":
			return "", fmt.Sprintf("Kustomization %s in %s has not applied anything yet", s.Name, s.Cluster), false
		case digest != "" && s.Revision != digest:
			return "", fmt.Sprintf("the env's Kustomizations disagree on what is applied (%s in %s reports %s, another reports %s)",
				s.Name, s.Cluster, shortDigest(s.Revision), shortDigest(digest)), false
		}
		digest = s.Revision
	}
	return digest, "", digest != ""
}

// fluxLiveShape resolves Live for a Flux-converged env, or reports that it is
// not a Flux env at all (handled=false) so the caller uses the apply history.
func fluxLiveShape(ctx context.Context, projectDir, env, candidateDigest string, bundles []release.BundleRecord) (live *release.Shape, basis release.PlanBasis, handled bool) {
	entities, err := RenderKCL(ctx, projectDir, env)
	if err != nil || !reconcilesThroughFlux(entities, envLedger{}) {
		return nil, release.PlanBasis{}, false
	}
	doc, err := fluxBundleDocFromLayout(ctx, projectDir, candidateDigest)
	if err != nil {
		return nil, release.PlanBasis{}, false
	}
	live, basis = fluxLiveFromCluster(ctx, env, entities, doc, bundles)
	return live, basis, true
}

// fluxLiveFromCluster maps the cluster's agreed revision onto a recorded
// bundle. Nil means Live is unknown.
func fluxLiveFromCluster(ctx context.Context, env string, entities *KCLEntities, doc release.BundleDoc, bundles []release.BundleRecord) (*release.Shape, release.PlanBasis) {
	revision, _, ok := fluxAppliedRevision(ctx, env, entities, doc)
	if !ok {
		return nil, release.PlanBasis{}
	}
	applied, found := bundleWithDigest(bundles, revision)
	if !found {
		return nil, release.PlanBasis{}
	}
	shape := applied.Shape
	return &shape, release.PlanBasis{AppliedBundleID: applied.ID}
}
