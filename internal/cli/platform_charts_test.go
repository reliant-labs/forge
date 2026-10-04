package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/flux"
	"github.com/reliant-labs/forge/pkg/release"
)

func platformTestEnv() (*KCLEntities, []ClusterEntity) {
	clusters := []ClusterEntity{fluxTestCluster("a")}
	e := reconciledEntities(clusters...)
	e.HelmCharts = []HelmChartEntity{
		{Name: "cnpg", OCI: "oci://ghcr.io/cloudnative-pg/charts/cloudnative-pg", Version: "1", Namespace: "cnpg"},
		{Name: "envoy-gateway", OCI: "oci://docker.io/envoyproxy/gateway-helm", Version: "1", Namespace: "eg", CRDs: "gateway-api"},
		// Not named "flux": detection is by chart reference.
		{Name: "gitops", OCI: flux.ChartOCI, Version: flux.ChartVersion, Namespace: flux.Namespace},
	}
	return e, clusters
}

func captureChartInstalls(t *testing.T) *[]string {
	t.Helper()
	var got []string
	restore := applyPlatformChartFn
	applyPlatformChartFn = func(_ context.Context, kctx string, spec cluster.HelmChartSpec) error {
		got = append(got, spec.Name+"@"+kctx)
		return nil
	}
	t.Cleanup(func() { applyPlatformChartFn = restore })
	return &got
}

// `cluster up` installs the DECLARED Flux (previously skipped by forge's own
// install and never replaced), Flux first and CRD providers before consumers.
func TestEnsureEnvPlatformChartsInstalled_InstallsDeclaredFluxFirst(t *testing.T) {
	got := captureChartInstalls(t)
	e, clusters := platformTestEnv()
	if err := ensureEnvPlatformChartsInstalled(context.Background(), "e2e", e, clusters); err != nil {
		t.Fatal(err)
	}
	want := []string{"gitops@k3d-a", "envoy-gateway@k3d-a", "cnpg@k3d-a"}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("installs = %v, want %v", *got, want)
	}
}

func TestEnsureEnvPlatformChartsInstalled_SkipsNonK3dClusters(t *testing.T) {
	got := captureChartInstalls(t)
	e, _ := platformTestEnv()
	clusters := []ClusterEntity{{Name: "prod", Context: "k3d-a", Provider: "gke"}}
	if err := ensureEnvPlatformChartsInstalled(context.Background(), "e2e", e, clusters); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 0 {
		t.Errorf("installed %v into a cluster forge did not create", *got)
	}
}

// `env deploy --target <chart>` installs that chart and writes no pointer.
func TestFollowPromote_TargetPlatformChartInstallsWithoutPointer(t *testing.T) {
	got := captureChartInstalls(t)
	e, _ := platformTestEnv()
	charts, rest := splitPlatformChartTargets(e, []string{"cnpg"})
	if !reflect.DeepEqual(charts, []string{"cnpg"}) || len(rest) != 0 {
		t.Fatalf("split = %v / %v", charts, rest)
	}
	if err := installPlatformCharts(context.Background(), e, platformChartInstalls(e, charts, nil)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got, []string{"cnpg@k3d-a"}) {
		t.Errorf("installs = %v", *got)
	}
	if c, r := splitPlatformChartTargets(e, []string{"api"}); len(c) != 0 || len(r) != 1 {
		t.Errorf("an app target must not be taken for a chart: %v / %v", c, r)
	}
}

func shapeObj(kind, name, hash string) release.ShapeObject {
	return release.ShapeObject{Cluster: "k3d-a", Kind: kind, Name: name, Namespace: "app", Hash: hash}
}

func fakeDigest(c string) string { return "sha256:" + strings.Repeat(c, 64) }

func stubFluxRevisions(t *testing.T, revisions ...string) {
	t.Helper()
	restore := fluxObserve
	fluxObserve = func(_ context.Context, ptrs []flux.Pointer, now time.Time) (flux.Observation, error) {
		obs := flux.Observation{At: now}
		for i, p := range ptrs {
			for _, k := range p.Kustomizations {
				obs.Statuses = append(obs.Statuses, flux.Status{
					Name: k.Name, Cluster: k.Cluster, Found: true, Ready: true, Revision: revisions[i%len(revisions)],
				})
			}
		}
		return obs, nil
	}
	t.Cleanup(func() { fluxObserve = restore })
}

func fluxLiveFixture() (*KCLEntities, release.BundleDoc, []release.BundleRecord) {
	e, _ := platformTestEnv()
	doc := release.BundleDoc{ClusterPaths: []release.BundleClusterTree{{Cluster: "k3d-a", Path: "clusters/k3d-a", Documents: 3}}}
	applied := release.BundleRecord{ID: "b1", Digest: fakeDigest("a"), Shape: release.Shape{
		Kind: release.EnvKind("self_managed"),
		Objects: []release.ShapeObject{
			shapeObj("ConfigMap", "cfg", fakeDigest("1")),
			shapeObj("PersistentVolumeClaim", "data", fakeDigest("2")),
			shapeObj("Deployment", "api", fakeDigest("3")),
		},
	}}
	return e, doc, []release.BundleRecord{applied}
}

// Live = the bundle whose digest equals the Kustomization's lastAppliedRevision.
func TestFluxLiveFromCluster_ConvergedBundleIsLive(t *testing.T) {
	stubFluxRevisions(t, fakeDigest("a"))
	e, doc, bundles := fluxLiveFixture()
	live, basis := fluxLiveFromCluster(context.Background(), "e2e", e, doc, bundles)
	if live == nil || basis.AppliedBundleID != "b1" {
		t.Fatalf("live = %v basis = %+v, want bundle b1", live, basis)
	}
}

// A removed ConfigMap is a removal finding; a removed PVC is stop-class.
func TestFluxLive_RemovalsProduceFindings(t *testing.T) {
	stubFluxRevisions(t, fakeDigest("a"))
	e, doc, bundles := fluxLiveFixture()
	live, _ := fluxLiveFromCluster(context.Background(), "e2e", e, doc, bundles)

	candidate := release.Shape{Kind: live.Kind, Objects: []release.ShapeObject{
		shapeObj("Deployment", "api", fakeDigest("3")),
	}}
	plan, err := release.BuildPlan(release.PlanInput{Candidate: candidate, Live: live})
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]release.FindingClass{}
	for _, f := range plan.Findings {
		codes[f.Code] = f.Class
	}
	if codes[release.FindingObjectRemoved] != release.ClassWarn {
		t.Errorf("ConfigMap removal finding missing: %+v", plan.Findings)
	}
	if codes[release.FindingStatefulDeletion] != release.ClassStop {
		t.Errorf("PVC removal must be stop-class: %+v", plan.Findings)
	}
	if codes[release.FindingUnknown] == release.ClassWarn {
		for _, f := range plan.Findings {
			if f.Code == release.FindingUnknown && f.Section == release.SectionObjects {
				t.Errorf("live must be known: %+v", f)
			}
		}
	}
}

// Disagreeing Kustomizations, or a revision with no recorded bundle: unknown.
func TestFluxLiveFromCluster_UnknownWhenItCannotBeSaid(t *testing.T) {
	e, doc, bundles := fluxLiveFixture()
	e.Clusters = append(e.Clusters, fluxTestCluster("b"))
	doc.ClusterPaths = append(doc.ClusterPaths, release.BundleClusterTree{Cluster: "k3d-b", Path: "clusters/k3d-b", Documents: 1})

	stubFluxRevisions(t, fakeDigest("a"), fakeDigest("b"))
	if live, _ := fluxLiveFromCluster(context.Background(), "e2e", e, doc, bundles); live != nil {
		t.Error("clusters disagree: Live must be unknown, not guessed")
	}
	stubFluxRevisions(t, fakeDigest("c"))
	if live, _ := fluxLiveFromCluster(context.Background(), "e2e", e, doc, bundles); live != nil {
		t.Error("a revision no recorded bundle matches must read as unknown")
	}
}
