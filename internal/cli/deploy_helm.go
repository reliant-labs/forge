// Package cli — bridge from declared platform deps (forge.HelmChart) to
// the cluster apply pipeline's helm-as-a-RENDERER path.
//
// The KCL layer projects each forge.HelmChart into the `output.helm_charts`
// contract (HelmChartEntity). This file turns those declarations into
// internal/cluster.HelmChartSpec values — fetching the forge-supplied CRD
// bundle each chart needs (the pinned standard Gateway API CRDs for Envoy
// Gateway; cert-manager's CRDs at the chart version) so cluster.Apply can
// apply CRDs-first/Established-gated before the chart's --skip-crds
// controllers.
//
// The CRD-fetch REUSES the existing pinned-CRD machinery
// (dev_cluster_ingress.go: ingressPinnedVersions + the cached fetch) that
// the k3d `forge cluster up` path already owns — keeping internal/cluster
// free of the templates/cache dependency (cli imports cluster, not the
// reverse).
package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/cluster"
)

// marshalManifestDicts serialises a list of raw manifest dicts (the KCL
// `output.helm_charts[].manifests` projection — GatewayClass / ClusterIssuers
// riding a chart's --target) into the `---`-separated multi-doc YAML stream
// cluster.Apply stamps with the chart's app-label and applies after the
// chart's controllers. Empty in => empty stream (the common no-manifests case).
func marshalManifestDicts(mans []any) (string, error) {
	if len(mans) == 0 {
		return "", nil
	}
	var sb strings.Builder
	for i, m := range mans {
		if i > 0 {
			sb.WriteString("---\n")
		}
		b, err := yaml.Marshal(m)
		if err != nil {
			return "", fmt.Errorf("marshal manifest %d: %w", i, err)
		}
		sb.Write(b)
	}
	return sb.String(), nil
}

// selectedHelmChartEntities returns the declared charts THIS `--target`
// renders, applying the SAME uniform exclusive rule as
// cluster.selectHelmChartsByGroup (which re-applies it inside Apply): a
// chart's NAME is its GROUP, so the result is EVERY chart when targets is
// empty (a bare `forge deploy <env>` reconciles every declared platform
// dep — the full declarative reconcile), and EXACTLY the charts whose Name
// ∈ targets otherwise. Pre-selecting here keeps the CRD-bundle network
// fetch scoped to the charts the apply will actually render.
func selectedHelmChartEntities(charts []HelmChartEntity, targets []string) []HelmChartEntity {
	if len(targets) == 0 {
		return charts
	}
	want := map[string]struct{}{}
	for _, t := range targets {
		want[t] = struct{}{}
	}
	var out []HelmChartEntity
	for _, c := range charts {
		if _, ok := want[c.Name]; ok {
			out = append(out, c)
		}
	}
	return out
}

// helmChartSpecsFromEntities resolves the env's declared platform deps
// (entities.HelmCharts) into cluster.HelmChartSpec values ready for
// cluster.Apply, fetching each chart's forge-supplied CRD bundle. Returns
// nil for an env that declares no charts (the common case) so the apply
// path is byte-identical for app-only envs.
//
// CRD bundle selection (HelmChartEntity.CRDs), each Established-gated by
// cluster.Apply before the chart's controllers:
//   - ""             — no forge CRDs (the chart needs none).
//   - "gateway-api"  — the pinned standard-channel Gateway API CRDs
//     (gateway_api= in internal/templates/ingress/envoy/VERSION), the
//     SAME bundle the k3d ingress install applies; matches the cloud
//     surface and activates the safe-upgrades policy that makes the
//     chart's bundled (skipped) experimental CRDs the wrong source.
//   - "cert-manager" — cert-manager's CRDs at the chart's OWN Version
//     (the chart and its CRDs move in lockstep).
func helmChartSpecsFromEntities(ctx context.Context, charts []HelmChartEntity) ([]cluster.HelmChartSpec, error) {
	if len(charts) == 0 {
		return nil, nil
	}
	specs := make([]cluster.HelmChartSpec, 0, len(charts))
	for _, c := range charts {
		crds, err := fetchHelmChartCRDs(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("platform dependency %q: %w", c.Name, err)
		}
		// Consumer-declared raw manifests (GatewayClass / ClusterIssuers)
		// that ride this chart's --target: serialise the dicts into the
		// `---`-separated stream cluster.Apply stamps + applies after the
		// chart's controllers.
		extra, err := marshalManifestDicts(c.Manifests)
		if err != nil {
			return nil, fmt.Errorf("platform dependency %q manifests: %w", c.Name, err)
		}
		specs = append(specs, cluster.HelmChartSpec{
			Name:      c.Name,
			Chart:     c.Chart,
			Repo:      c.Repo,
			OCI:       c.OCI,
			Version:   c.Version,
			Namespace: c.Namespace,
			Values:    c.Values,
			CRDs:      crds,
			Manifests: extra,
		})
	}
	return specs, nil
}

// fetchHelmChartCRDs returns the forge-supplied CRD manifest YAML for a
// chart, per its declared CRD bundle. Empty string when the chart needs no
// forge CRDs.
func fetchHelmChartCRDs(ctx context.Context, c HelmChartEntity) (string, error) {
	switch c.CRDs {
	case "":
		return "", nil
	case "gateway-api":
		// REUSE the pinned standard Gateway API CRDs the k3d ingress path
		// owns — same version, same cached fetch.
		_, gatewayAPIVer, err := ingressPinnedVersions()
		if err != nil {
			return "", err
		}
		path, err := fetchGatewayAPICRDs(ctx, gatewayAPIVer)
		if err != nil {
			return "", err
		}
		return readFileString(path)
	case "cert-manager":
		// cert-manager publishes its CRDs at the chart version; fetch +
		// cache them with the same machinery as the Gateway API CRDs.
		path, err := fetchCertManagerCRDs(ctx, c.Version)
		if err != nil {
			return "", err
		}
		return readFileString(path)
	default:
		return "", fmt.Errorf("unknown crds bundle %q (want '', 'gateway-api', or 'cert-manager')", c.CRDs)
	}
}

// readFileString reads a file into a string — the CRD YAML cluster.Apply
// applies as the first (Established-gated) pass.
func readFileString(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read CRD bundle %s: %w", path, err)
	}
	return string(b), nil
}
