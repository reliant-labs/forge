package cli

// `forge cluster up <env>` installs the RECONCILER into every cluster that
// needs one.
//
// WHY THE CLUSTER PHASE AND NOT THE DEPLOY. Flux has to be running before a
// pointer written at it means anything: an OCIRepository applied to a cluster
// with no source-controller is admitted (once the CRDs exist) and then simply
// sits there, and a Kustomization with no kustomize-controller never reports a
// condition at all — so a deploy would write its pointer successfully and the
// wait would time out with nothing to diagnose. Installing it with the cluster
// puts the reconciler in place at the same moment the cluster exists, which is
// also the ordering CI needs: it creates clusters ahead of the deploy so the
// kubectl steps in between have a target.
//
// WHICH CLUSTERS. Every forge-managed k3d cluster that hosts a CLUSTER env
// with no control plane and no declared lifecycle — the exact envs
// [reconcilesThroughFlux] routes through the pointer path. The three
// conditions are read from the same predicates the deploy reads, never
// re-derived, because an env that got a reconciler installed but not a pointer
// (or the reverse) is worse off than one that got neither: it would look
// configured and converge nothing.
//
// THE SHARED-FLUX RULE, which is the part that bit control-plane. An env may
// DECLARE Flux itself, as a forge.HelmChart, because it needs one for its own
// reasons — cp's `e2e` runs Flux for hosted tenants. Installing a second one
// into the same cluster does not fail cleanly: both charts own the same
// cluster-scoped CRDs and the same `flux-system` namespace, so whichever
// applies last takes the CRDs and the other's Kustomizations reconcile against
// a controller that was replaced underneath them. So forge installs Flux into
// a cluster ONLY when no declared chart already provides it.
//
// DETECTION IS BY CHART REFERENCE, NEVER BY NAME. A HelmChart's `name` is the
// `--target` selector its author picks freely — cp calls its chart "flux",
// another consumer might call it "gitops" — so a name heuristic would let a
// naming choice decide whether forge installed a colliding second install.
// [flux.ChartProvidedBy] compares the declared `oci` against the pinned chart
// reference, which is the fact that actually determines what is in the
// cluster. A consumer that wants to own Flux declares `forge.flux_chart()`
// (optionally with its own `values` for registry identity), and is recognised
// by that.
//
// IDEMPOTENT, because the install is a render-and-apply of a version-pinned
// chart through forge's ordinary chart pipeline: a warm run re-applies the
// same manifests and kubectl reports them unchanged. There is no
// install-if-absent check, deliberately — a check would have to decide what
// "already installed" means from the live cluster, and a half-installed Flux
// (CRDs present, Deployment missing) would read as installed and never be
// repaired.

import (
	"context"
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/flux"
)

// ensureEnvFluxInstalled installs forge's Flux into each of the env's declared
// k3d clusters that needs it.
//
// A no-op — and SILENT — for every env that does not reconcile: a `dev` env
// declaring `lifecycle = "local"` is the common case, and a line about a
// reconciler it deliberately does not have would be noise on every
// `forge env up`.
func ensureEnvFluxInstalled(ctx context.Context, env string, entities *KCLEntities, clusters []ClusterEntity) error {
	targets := fluxInstallTargets(entities, clusters)
	if len(targets) == 0 {
		return nil
	}
	fmt.Printf("\n[up] reconciler phase — installing Flux into %d cluster(s) that reconcile env %s from its bundle\n",
		len(targets), env)
	for _, kctx := range targets {
		if err := installFluxFn(ctx, kctx); err != nil {
			return fmt.Errorf("install Flux into cluster %q: %w", kctx, err)
		}
	}
	return nil
}

// fluxInstallTargets is every cluster forge installs Flux into: the env's
// declared k3d clusters, minus any that a declared chart already serves.
//
// It returns nothing at all for an env that does not reconcile, so the
// decision is made once, here, rather than checked again per cluster.
func fluxInstallTargets(entities *KCLEntities, clusters []ClusterEntity) []string {
	if entities == nil || len(clusters) == 0 {
		return nil
	}
	// Not reconciled => no reconciler. This reads the deploy's own
	// predicate, with a machine ledger asserted: an env declaring a
	// control plane has a version store that drives a reconciler already,
	// and its clusters are not forge's to install into.
	if entities.ControlPlane != nil || DirectApplyAllowed(entities) {
		return nil
	}
	provided := fluxProvidedClusters(entities, clusters)
	var out []string
	for _, c := range clusters {
		// Only a k3d cluster. A declared cluster on another provider is
		// one forge did not create and cannot delete, so installing a
		// cluster-scoped controller into it would be forge taking
		// ownership of infrastructure it was only pointed at.
		if c.Provider != "" && c.Provider != "k3d" {
			continue
		}
		kctx := strings.TrimSpace(c.Context)
		if kctx == "" || provided[kctx] {
			continue
		}
		out = append(out, kctx)
	}
	return out
}

// fluxProvidedClusters is the set of contexts where a DECLARED chart already
// provides Flux.
//
// A chart with no `cluster` installs into the env's primary cluster, which for
// this purpose is every cluster the env declares: the chart's manifests go to
// the env-wide context, and we cannot know from here which single cluster that
// resolved to without re-running the deploy's resolution. Treating an
// unretargeted Flux declaration as covering everything is the SAFE direction —
// it means forge installs none, and the consumer's own chart is the one that
// lands. The unsafe direction would be installing a second Flux beside the
// consumer's, which is the collision this whole rule exists to prevent.
func fluxProvidedClusters(entities *KCLEntities, clusters []ClusterEntity) map[string]bool {
	out := map[string]bool{}
	for _, c := range entities.HelmCharts {
		if !flux.ChartProvidedBy(c.OCI, c.Repo) {
			continue
		}
		if target := strings.TrimSpace(c.Cluster); target != "" {
			out[target] = true
			continue
		}
		for _, declared := range clusters {
			if kctx := strings.TrimSpace(declared.Context); kctx != "" {
				out[kctx] = true
			}
		}
	}
	return out
}

// installFlux renders and applies the pinned Flux chart into one cluster.
//
// IT GOES THROUGH forge's ORDINARY CHART PIPELINE — the same
// render-then-CRD-first-apply every declared forge.HelmChart flows through —
// rather than shelling out to `helm install`. Three things follow, and the
// third is why it matters here specifically:
//
//  1. helm stays a RENDERER, with no helm-managed release in the cluster for
//     anything to drift against;
//  2. the chart's CRDs are promoted into the early batch and gated on
//     Established before the controller Deployments. The community chart ships
//     its CRDs as templates, so `--skip-crds` does not drop them;
//  3. an OCIRepository written minutes later needs those CRDs Established. A
//     `helm install` that returned as soon as the release was recorded would
//     let a deploy write a pointer against a kind the apiserver has not
//     registered, and the apply would fail naming the kind rather than the
//     race.
//
// A var so the cluster phase's decision — which contexts get a reconciler —
// is testable without a cluster or a helm pull, which is the thing the
// shared-Flux rule needs pinned.
var installFluxFn = installFlux

func installFlux(ctx context.Context, kctx string) error {
	fmt.Printf("  installing Flux %s (chart %s) into %s...\n", flux.ChartVersion, flux.ChartOCI, kctx)
	spec := cluster.HelmChartSpec{
		Name:      flux.ChartName,
		OCI:       flux.ChartOCI,
		Version:   flux.ChartVersion,
		Namespace: flux.Namespace,
		Values:    fluxChartValues(),
		Cluster:   kctx,
	}
	return applyFluxChartFn(ctx, kctx, spec)
}

// applyFluxChartFn renders the chart and applies it, CRDs first.
var applyFluxChartFn = cluster.ApplyHelmChart

// fluxChartValues is the Go spelling of kcl/lib/flux.k's `_FLUX_VALUES`.
//
// TWO SPELLINGS OF ONE POSTURE, and that is a real cost worth naming. forge
// installs Flux from HERE — the cluster phase has no Bundle to read a chart
// off, because the whole point is that it runs before any deploy renders one —
// while a CONSUMER declaring Flux for its own reasons gets the values from
// `forge.flux_chart()`. The two must agree, and nothing in the type system
// makes them.
//
// They are kept honest by test rather than by construction:
// internal/cli/cluster_flux_test.go renders the KCL component and compares it
// to this map, so a flag added to one and not the other fails the build. The
// alternative — rendering KCL from the cluster phase to get the values — would
// make installing a reconciler depend on the env's render succeeding, which is
// the dependency this phase exists to not have.
func fluxChartValues() map[string]any {
	return map[string]any{
		"kustomizeController": map[string]any{
			"container": map[string]any{
				"additionalArgs": []any{"--no-remote-bases=true", "--no-cross-namespace-refs=true"},
			},
		},
		"helmController":            map[string]any{"create": false},
		"notificationController":    map[string]any{"create": false},
		"imageAutomationController": map[string]any{"create": false},
		"imageReflectionController": map[string]any{"create": false},
	}
}
