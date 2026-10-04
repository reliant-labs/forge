package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/flux"
)

// clusterEntity is the shorthand these tests declare clusters with.
func fluxTestCluster(name string) ClusterEntity {
	return ClusterEntity{Name: name, Context: "k3d-" + name}
}

// reconciledEntities is an env shaped like the ones this path serves: a
// cluster workload, no control plane, no declared lifecycle.
func reconciledEntities(clusters ...ClusterEntity) *KCLEntities {
	e := &KCLEntities{Clusters: clusters}
	if len(clusters) > 0 {
		e.ClusterTarget = &ClusterTargetEntity{Cluster: clusters[0].Context, Namespace: "app"}
		e.Workloads = []WorkloadEntity{fluxTestWorkload(clusters[0].Context, "localhost:5050/p/api:1")}
	}
	return e
}

// TestFluxInstallTargets_OnlyEnvsThatActuallyReconcile pins that the
// reconciler is installed for exactly the envs whose deploy writes a pointer.
//
// Both directions are failures worth naming. Installing where no pointer is
// written leaves a controller running with nothing to converge — harmless but
// confusing, and it is attack surface nobody asked for. NOT installing where a
// pointer IS written is worse: the deploy writes its pointer successfully and
// the wait times out with nothing to diagnose, because an OCIRepository with
// no source-controller is simply admitted and then sits there.
func TestFluxInstallTargets_OnlyEnvsThatActuallyReconcile(t *testing.T) {
	t.Parallel()
	clusters := []ClusterEntity{fluxTestCluster("a")}

	for _, tc := range []struct {
		name     string
		entities *KCLEntities
		want     []string
	}{
		{
			name:     "no lifecycle, no control plane, targets a cluster — reconciled",
			entities: reconciledEntities(clusters...),
			want:     []string{"k3d-a"},
		},
		{
			// A developer's own cluster. Direct apply IS the point
			// there, so a reconciler would be a control loop fighting
			// the inner loop.
			name: "declares lifecycle = local",
			entities: func() *KCLEntities {
				e := reconciledEntities(clusters...)
				e.Lifecycle = lifecycleLocal
				return e
			}(),
		},
		{
			name: "declares lifecycle = ephemeral",
			entities: func() *KCLEntities {
				e := reconciledEntities(clusters...)
				e.Lifecycle = lifecycleEphemeral
				return e
			}(),
		},
		{
			// A control plane is a version store that already drives
			// a reconciler; these clusters are not forge's to install
			// into.
			name: "declares a control plane",
			entities: func() *KCLEntities {
				e := reconciledEntities(clusters...)
				e.ControlPlane = &ControlPlaneEntity{}
				return e
			}(),
		},
		{
			// Nothing for a reconciler to converge.
			name:     "targets no cluster",
			entities: &KCLEntities{Clusters: clusters},
		},
	} {
		got := fluxInstallTargets(tc.entities, clusters)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: fluxInstallTargets = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestFluxInstallTargets_SkipsAClusterADeclaredChartAlreadyServes is THE
// SHARED-FLUX RULE, and it is the assertion that keeps control-plane working.
//
// cp's `e2e` env installs its OWN Flux into its control-plane cluster, for
// hosted tenants. A second install does not fail cleanly: both charts own the
// same cluster-scoped CRDs and the same `flux-system` namespace, so whichever
// applies last takes the CRDs and the other's Kustomizations reconcile against
// a controller that was replaced underneath them.
//
// DETECTION IS BY CHART REFERENCE, NEVER BY NAME. The chart below is called
// "gitops", not "flux" — a name heuristic would miss it and install the
// colliding second copy.
func TestFluxInstallTargets_SkipsAClusterADeclaredChartAlreadyServes(t *testing.T) {
	t.Parallel()
	clusters := []ClusterEntity{fluxTestCluster("a"), fluxTestCluster("b")}

	t.Run("a retargeted chart covers only its own cluster", func(t *testing.T) {
		e := reconciledEntities(clusters...)
		e.HelmCharts = []HelmChartEntity{{
			// Deliberately NOT named "flux".
			Name: "gitops", OCI: flux.ChartOCI, Version: flux.ChartVersion,
			Namespace: flux.Namespace, Cluster: "k3d-a",
		}}
		got := fluxInstallTargets(e, clusters)
		if !reflect.DeepEqual(got, []string{"k3d-b"}) {
			t.Errorf("targets = %v, want only k3d-b — k3d-a's Flux is the consumer's, and a second "+
				"install would collide on its CRDs and flux-system", got)
		}
	})

	t.Run("an unretargeted chart covers every declared cluster", func(t *testing.T) {
		// A chart with no `cluster` installs into the env's primary,
		// and we cannot tell from here which that resolved to. Treating
		// it as covering everything is the SAFE direction: forge
		// installs none, and the consumer's own chart lands. The unsafe
		// direction would be installing a second Flux beside it.
		e := reconciledEntities(clusters...)
		e.HelmCharts = []HelmChartEntity{{
			Name: "flux", OCI: flux.ChartOCI, Version: flux.ChartVersion, Namespace: flux.Namespace,
		}}
		if got := fluxInstallTargets(e, clusters); len(got) != 0 {
			t.Errorf("targets = %v, want none", got)
		}
	})

	t.Run("a DIFFERENT chart does not provide Flux", func(t *testing.T) {
		e := reconciledEntities(clusters...)
		e.HelmCharts = []HelmChartEntity{
			{Name: "envoy-gateway", OCI: "oci://docker.io/envoyproxy/gateway-helm", Version: "v1.7.2", Namespace: "envoy-gateway-system"},
			{Name: "cert-manager", Chart: "cert-manager", Repo: "https://charts.jetstack.io", Version: "v1.20.1", Namespace: "cert-manager"},
		}
		got := fluxInstallTargets(e, clusters)
		if !reflect.DeepEqual(got, []string{"k3d-a", "k3d-b"}) {
			t.Errorf("targets = %v, want both clusters — neither chart is Flux", got)
		}
	})
}

// TestFluxInstallTargets_OnlyK3dClusters pins that forge does not install a
// cluster-scoped controller into a cluster it did not create.
//
// A declared cluster on another provider is one forge was only POINTED at, and
// cannot delete. Installing Flux into it would be forge taking ownership of
// infrastructure somebody else operates.
func TestFluxInstallTargets_OnlyK3dClusters(t *testing.T) {
	t.Parallel()
	clusters := []ClusterEntity{
		fluxTestCluster("a"),
		{Name: "prod", Context: "gke_proj_us-central1_prod", Provider: "gke"},
	}
	got := fluxInstallTargets(reconciledEntities(clusters...), clusters)
	if !reflect.DeepEqual(got, []string{"k3d-a"}) {
		t.Errorf("targets = %v, want only the k3d cluster", got)
	}
}

// TestEnsureEnvFluxInstalled_InstallsOncePerTargetCluster pins that the
// install runs for each cluster that needs one and is skipped entirely
// otherwise — including that it prints nothing for a `local` env, which is
// the common case and must not grow a line on every `forge env up`.
func TestEnsureEnvFluxInstalled_InstallsOncePerTargetCluster(t *testing.T) {
	t.Parallel()
	clusters := []ClusterEntity{fluxTestCluster("a"), fluxTestCluster("b")}

	var installed []string
	restore := installFluxFn
	installFluxFn = func(_ context.Context, kctx string) error {
		installed = append(installed, kctx)
		return nil
	}
	t.Cleanup(func() { installFluxFn = restore })

	if err := ensureEnvFluxInstalled(context.Background(), "dev-k8s", reconciledEntities(clusters...), clusters); err != nil {
		t.Fatalf("ensureEnvFluxInstalled: %v", err)
	}
	if !reflect.DeepEqual(installed, []string{"k3d-a", "k3d-b"}) {
		t.Errorf("installed = %v, want both clusters once each", installed)
	}

	installed = nil
	local := reconciledEntities(clusters...)
	local.Lifecycle = lifecycleLocal
	if err := ensureEnvFluxInstalled(context.Background(), "dev", local, clusters); err != nil {
		t.Fatalf("ensureEnvFluxInstalled(local): %v", err)
	}
	if len(installed) != 0 {
		t.Errorf("installed %v for a lifecycle=local env; direct apply is the point there", installed)
	}
}

// TestInstallFlux_UsesThePinnedChartThroughForgesOwnPipeline pins the three
// facts a pointer written minutes later depends on.
func TestInstallFlux_UsesThePinnedChartThroughForgesOwnPipeline(t *testing.T) {
	t.Parallel()
	var got cluster.HelmChartSpec
	var gotCtx string
	restore := applyFluxChartFn
	applyFluxChartFn = func(_ context.Context, kctx string, spec cluster.HelmChartSpec) error {
		gotCtx, got = kctx, spec
		return nil
	}
	t.Cleanup(func() { applyFluxChartFn = restore })

	if err := installFlux(context.Background(), "k3d-a"); err != nil {
		t.Fatalf("installFlux: %v", err)
	}
	if gotCtx != "k3d-a" || got.Cluster != "k3d-a" {
		t.Errorf("applied to context %q / spec cluster %q, want k3d-a for both", gotCtx, got.Cluster)
	}
	if got.OCI != flux.ChartOCI || got.Version != flux.ChartVersion {
		t.Errorf("chart = %s@%s, want %s@%s", got.OCI, got.Version, flux.ChartOCI, flux.ChartVersion)
	}
	if got.Namespace != flux.Namespace {
		t.Errorf("namespace = %q, want %q — the pointer is written into this namespace by name",
			got.Namespace, flux.Namespace)
	}
	// forge supplies no CRD bundle for Flux: the community chart ships its
	// CRDs as templates, so --skip-crds does not drop them and forge's
	// CRD-first apply gates them on Established itself. Naming a bundle
	// would ask forge for CRDs it does not own.
	if got.CRDBundle != "" {
		t.Errorf("CRDBundle = %q, want empty", got.CRDBundle)
	}
}

// TestFluxChartValues_MatchesTheKCLComponent is the test that keeps TWO
// SPELLINGS OF ONE POSTURE honest.
//
// forge installs Flux from Go, during the cluster phase, because that phase
// runs before any deploy has rendered a Bundle to read a chart off. A CONSUMER
// declaring Flux for its own reasons gets the values from
// `forge.flux_chart()`. Nothing in the type system makes the two agree, so a
// flag added to one and not the other would mean forge's own install and a
// consumer's differ in their security posture — silently, and in whichever
// direction the omission fell.
//
// This renders the KCL component and compares it field for field. It is
// deliberately an EQUALITY check rather than a subset: a value present in one
// and absent from the other is exactly the drift being guarded against.
func TestFluxChartValues_MatchesTheKCLComponent(t *testing.T) {
	t.Parallel()
	rendered := renderKCLFluxChartValues(t)
	goSide := fluxChartValues()
	if !reflect.DeepEqual(normalizeValues(goSide), normalizeValues(rendered)) {
		t.Errorf("forge's own Flux install and kcl/lib/flux.k's forge.flux_chart() disagree about the "+
			"chart values.\n  Go  (internal/cli/cluster_flux.go fluxChartValues): %#v\n  KCL (kcl/lib/flux.k _FLUX_VALUES):        %#v\n"+
			"  These are two spellings of one security posture; update both.", normalizeValues(goSide), normalizeValues(rendered))
	}
}

// TestFluxChartValues_KeepsTheSecurityPosture pins each flag against what it
// is FOR, so a future edit that drops one has to argue with the reason rather
// than with a struct literal.
func TestFluxChartValues_KeepsTheSecurityPosture(t *testing.T) {
	t.Parallel()
	v := fluxChartValues()
	// No multitenancy block: it makes kustomize-controller impersonate
	// flux-system:default, which cannot apply the Namespaces and CRDs the
	// env's own bundle carries. Measured against a real cluster.
	if _, present := v["multitenancy"]; present {
		t.Error("multitenancy is set; with forge as the only writer it makes every apply fail Forbidden")
	}
	// The apply is hermetic and a Kustomization may only name its own
	// namespace's source.
	args := v["kustomizeController"].(map[string]any)["container"].(map[string]any)["additionalArgs"]
	if !reflect.DeepEqual(args, []any{"--no-remote-bases=true", "--no-cross-namespace-refs=true"}) {
		t.Errorf("kustomizeController args = %v; want hermetic + no cross-namespace refs", args)
	}
	// Two controllers, not six. Each of these is attack surface with
	// nothing on this path to justify it; helm-controller specifically
	// carries a cross-tenant DoS history (CVE-2022-36049).
	for _, off := range []string{"helmController", "notificationController", "imageAutomationController", "imageReflectionController"} {
		block, ok := v[off].(map[string]any)
		if !ok || block["create"] != false {
			t.Errorf("%s.create is not false; only source- and kustomize-controller are on this path", off)
		}
	}
}

// normalizeValues makes two nested value maps comparable regardless of whether
// a leaf list came from Go (`[]any{...}`) or from the KCL render
// (`[]interface{}{...}` of the same strings) — they are already the same type,
// but a bool arriving as a JSON number or a string would otherwise compare
// unequal for a reason that is not drift.
func normalizeValues(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch typed := v.(type) {
		case map[string]any:
			out[k] = normalizeValues(typed)
		case []any:
			list := make([]any, len(typed))
			copy(list, typed)
			out[k] = list
		default:
			out[k] = v
		}
	}
	return out
}

// TestClusterReachableRegistry_RewritesTheHOSTSNameToTHEPODS is the addressing
// mistake that reads as a registry outage.
//
// `localhost:<port>` is where the HOST pushes. Inside a pod it resolves to the
// pod itself, so an OCIRepository carrying it fails to fetch with a
// connection-refused naming localhost — which looks like the registry is down
// rather than like the address is wrong. `host.k3d.internal` is the Docker
// host-gateway alias forge keeps in every managed cluster's CoreDNS NodeHosts.
//
// `registry.localhost` is rewritten too, and that is the subtle one: forge's
// containerd mirror names it, but that alias is for the KUBELET, which resolves
// it through the node's /etc/hosts. A POD does not, and source-controller is a
// pod.
func TestClusterReachableRegistry_RewritesTheHOSTSNameToTHEPODS(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"localhost:5050/p/bundle.v1/dev", "host.k3d.internal:5050/p/bundle.v1/dev"},
		{"127.0.0.1:5050/p", "host.k3d.internal:5050/p"},
		{"registry.localhost:5000/p", "host.k3d.internal:5000/p"},
		{"localhost:5050", "host.k3d.internal:5050"},
		// A real registry is reachable by the same name everywhere;
		// rewriting one would be inventing an address.
		{"ghcr.io/reliant-labs/p/bundle.v1/prod", "ghcr.io/reliant-labs/p/bundle.v1/prod"},
		{"us-central1-docker.pkg.dev/proj/repo/p", "us-central1-docker.pkg.dev/proj/repo/p"},
		// Already the pods' name.
		{"host.k3d.internal:5050/p", "host.k3d.internal:5050/p"},
	} {
		if got := clusterReachableRegistry(tc.in); got != tc.want {
			t.Errorf("clusterReachableRegistry(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestReconcilesThroughFlux pins the routing predicate, which decides whether
// a deploy applies from this machine or writes a pointer.
func TestReconcilesThroughFlux(t *testing.T) {
	t.Parallel()
	clusters := []ClusterEntity{fluxTestCluster("a")}
	machine := envLedger{}
	hosted := envLedger{Hosted: true}

	if !reconcilesThroughFlux(reconciledEntities(clusters...), machine) {
		t.Error("an env with no lifecycle, no control plane and a cluster must reconcile")
	}
	if reconcilesThroughFlux(reconciledEntities(clusters...), hosted) {
		t.Error("a hosted env's version store already drives a reconciler; this path must not claim it")
	}
	local := reconciledEntities(clusters...)
	local.Lifecycle = lifecycleLocal
	if reconcilesThroughFlux(local, machine) {
		t.Error("a lifecycle=local env applies directly; that is the point of the declaration")
	}
	if reconcilesThroughFlux(&KCLEntities{}, machine) {
		t.Error("an env targeting no cluster has nothing for a reconciler to converge")
	}
}

func fluxTestWorkload(kctx, image string) WorkloadEntity {
	w := WorkloadEntity{
		Name:    "api",
		Runtime: RuntimeEntity{Type: RuntimeCluster, Cluster: &ClusterRuntime{Cluster: kctx}},
	}
	w.Spec.Image = image
	return w
}

// TestFluxRegistryBase_ReadsTheWorkloadsImage pins that the bundle is
// published beside the images it pins. An environment does not declare a
// registry; a workload does, as part of its image, so that is the only place
// to read one from.
func TestFluxRegistryBase_ReadsTheWorkloadsImage(t *testing.T) {
	t.Parallel()
	one := &KCLEntities{Workloads: []WorkloadEntity{fluxTestWorkload("k3d-a", "localhost:5050/acme/api:1")}}
	if got := fluxRegistryBase(one); got != "localhost:5050/acme" {
		t.Errorf("base = %q, want the image's registry and path minus its own name", got)
	}
	// A bare image (no host) names no registry; the next workload may.
	mixed := &KCLEntities{Workloads: []WorkloadEntity{
		fluxTestWorkload("k3d-a", "busybox"),
		fluxTestWorkload("k3d-a", "ghcr.io/acme/web@sha256:"+strings.Repeat("a", 64)),
	}}
	if got := fluxRegistryBase(mixed); got != "ghcr.io/acme" {
		t.Errorf("base = %q, want ghcr.io/acme", got)
	}
	if got := fluxRegistryBase(&KCLEntities{}); got != "" {
		t.Errorf("base = %q, want empty when no workload names a registry", got)
	}
}

// TestPublishBundleForFlux_RefusesWithoutARegistry pins that the refusal names
// the declaration to add. A cluster cannot fetch a bundle from a directory on
// the developer's laptop, and inventing a registry would point it at one that
// does not hold the bytes.
func TestPublishBundleForFlux_RefusesWithoutARegistry(t *testing.T) {
	t.Parallel()
	_, err := publishBundleForFlux(context.Background(), t.TempDir(), "dev-k8s", &KCLEntities{}, strings.Repeat("a", 0))
	if err == nil {
		t.Fatal("publishBundleForFlux succeeded with no declared registry")
	}
	if !strings.Contains(err.Error(), "registry") || !strings.Contains(err.Error(), "image") {
		t.Errorf("error = %v; it should name the declaration to add", err)
	}
}
