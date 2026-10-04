package flux

import (
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

const (
	testDigest  = "sha256:" + "ab" + "cdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	otherDigest = "sha256:" + "ff" + "cdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

func validDigest(t *testing.T, d string) string {
	t.Helper()
	if !release.ValidDigest(d) {
		t.Fatalf("test fixture %q is not a canonical digest; the test itself is wrong", d)
	}
	return d
}

// TestBuildPointer_OneKustomizationPerPathThatEXISTS is the load-bearing one.
//
// The failure it closes is the one that reports SUCCESS. A Kustomization whose
// `spec.path` is not in the artifact reconciles as "nothing to apply" and goes
// Ready=True — so an env built for a declared-but-empty cluster would look
// converged while running the previous release forever. BundleDoc.ClusterPaths
// lists the paths the layer actually HOLDS, and this pins that the builder
// reads that rather than the env's declared cluster list.
func TestBuildPointer_OneKustomizationPerPathThatEXISTS(t *testing.T) {
	t.Parallel()
	p, err := BuildPointer(PointerInput{
		Env: "dev-k8s", Repository: "k3d-reg:5000/p/bundle.v1/dev-k8s",
		Digest: validDigest(t, testDigest), Cluster: "k3d-a",
		ClusterPaths: []release.BundleClusterTree{
			{Cluster: "k3d-a", Path: release.BundleClusterPath("k3d-a"), Documents: 7},
			// Declared, routed nothing: NO Kustomization. An empty path
			// reconciles green over an env nothing applied.
			{Cluster: "k3d-a-empty", Path: release.BundleClusterPath("k3d-a-empty"), Documents: 0},
			// Another cluster's path: applying it here would write that
			// cluster's objects into this one.
			{Cluster: "k3d-b", Path: release.BundleClusterPath("k3d-b"), Documents: 3},
			// The unclustered tree: documents the deploy attributes to
			// no cluster, which nothing applies.
			{Cluster: "", Path: release.BundleClusterPath(""), Documents: 2},
		},
	})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	if len(p.Kustomizations) != 1 {
		var got []string
		for _, k := range p.Kustomizations {
			got = append(got, k.Name+"@"+k.Doc["spec"].(map[string]any)["path"].(string))
		}
		t.Fatalf("Kustomizations = %v; want exactly one, for k3d-a's non-empty path", got)
	}
	spec := p.Kustomizations[0].Doc["spec"].(map[string]any)
	if want := "./" + release.BundleClusterPath("k3d-a"); spec["path"] != want {
		t.Errorf("path = %v, want %q — the path must be release.BundleClusterPath's answer, "+
			"never a second spelling: a path the artifact lacks reconciles Ready over nothing", spec["path"], want)
	}
}

// TestBuildPointer_PinsByDigestWithTheLayerSelector pins the three things the
// WAIT depends on. If any of them is wrong the wait does not fail — it hangs
// on a comparison that can never be true, or passes on a release it never saw.
func TestBuildPointer_PinsByDigestWithTheLayerSelector(t *testing.T) {
	t.Parallel()
	p, err := BuildPointer(PointerInput{
		Env: "dev-k8s", Repository: "k3d-reg:5000/p/bundle.v1/dev-k8s",
		Digest: validDigest(t, testDigest), Cluster: "k3d-a",
		ClusterPaths: []release.BundleClusterTree{
			{Cluster: "k3d-a", Path: release.BundleClusterPath("k3d-a"), Documents: 1},
		},
	})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	spec := p.Source.Doc["spec"].(map[string]any)

	// BY DIGEST, never a tag: a tag would make "converged" mean "converged
	// to whatever that tag pointed at when source-controller last looked".
	ref := spec["ref"].(map[string]any)
	if ref["digest"] != testDigest {
		t.Errorf("ref = %v, want digest %q", ref, testDigest)
	}
	if _, tagged := ref["tag"]; tagged {
		t.Error("ref carries a tag; the pointer must pin immutable bytes so lastAppliedRevision can be compared to them")
	}

	// THE LAYER SELECTOR. Without it source-controller hands over the
	// tarball itself, the Kustomization's path does not exist inside it,
	// and the reconcile reports Ready having applied nothing.
	sel, ok := spec["layerSelector"].(map[string]any)
	if !ok {
		t.Fatalf("spec has no layerSelector: %v", spec)
	}
	if sel["mediaType"] != release.BundleManifestsLayer {
		t.Errorf("layerSelector.mediaType = %v, want %q", sel["mediaType"], release.BundleManifestsLayer)
	}
	if sel["operation"] != "extract" {
		t.Errorf("layerSelector.operation = %v, want extract", sel["operation"])
	}
}

// TestBuildPointer_PruneAndWaitAreOn pins the two Kustomization flags that
// make the bundle authoritative and the wait meaningful.
func TestBuildPointer_PruneAndWaitAreOn(t *testing.T) {
	t.Parallel()
	p, err := BuildPointer(PointerInput{
		Env: "e2e", Repository: "k3d-reg:5000/p/bundle.v1/e2e",
		Digest: validDigest(t, testDigest), Cluster: "k3d-a",
		ClusterPaths: []release.BundleClusterTree{
			{Cluster: "k3d-a", Path: release.BundleClusterPath("k3d-a"), Documents: 1},
		},
	})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	spec := p.Kustomizations[0].Doc["spec"].(map[string]any)
	if spec["prune"] != true {
		// Without prune a removed workload outlives the render that
		// declared it, and the bundle stops being authoritative.
		t.Error("prune is not true; the bundle must be authoritative over what it carries")
	}
	if spec["wait"] != true {
		// Without wait, Ready=True means "the YAML was accepted" —
		// which is before any new pod has served.
		t.Error("wait is not true; Ready must mean the rollout happened, not that the apply was admitted")
	}
	// NO targetNamespace: the render already stamps every namespaced
	// object, so setting one would silently relocate anything the env
	// deliberately placed elsewhere.
	if _, set := spec["targetNamespace"]; set {
		t.Error("targetNamespace is set; it would override the render for every object at once")
	}
}

// TestBuildPointer_RequestedAtCarriesTheWritesTimestamp pins that the
// annotation's VALUE moves. Flux re-reconciles when it CHANGES, so a constant
// would be applied once and be a no-op forever — making every deploy after the
// first wait out the poll interval for nothing.
func TestBuildPointer_RequestedAtCarriesTheWritesTimestamp(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	build := func(when time.Time) string {
		p, err := BuildPointer(PointerInput{
			Env: "dev-k8s", Repository: "k3d-reg:5000/p", Digest: validDigest(t, testDigest),
			Cluster: "k3d-a", RequestedAt: when,
			ClusterPaths: []release.BundleClusterTree{
				{Cluster: "k3d-a", Path: release.BundleClusterPath("k3d-a"), Documents: 1},
			},
		})
		if err != nil {
			t.Fatalf("BuildPointer: %v", err)
		}
		meta := p.Source.Doc["metadata"].(map[string]any)
		return meta["annotations"].(map[string]any)[AnnotationRequestedAt].(string)
	}
	first := build(at)
	if first != "2026-03-04T05:06:07Z" {
		t.Errorf("requestedAt = %q, want the caller's clock in RFC3339", first)
	}
	if second := build(at.Add(time.Minute)); second == first {
		t.Error("requestedAt did not change with the clock; Flux reconciles on the value CHANGING, " +
			"so a constant makes every deploy wait out the poll interval")
	}
}

// TestBuildPointer_InsecureOnlyForAPlaintextRegistry pins that `insecure` is
// DERIVED from the address and is absent otherwise. A caller able to pass it
// could downgrade a real registry's transport.
func TestBuildPointer_InsecureOnlyForAPlaintextRegistry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		insecure bool
	}{
		{"insecure k3d registry", true},
		{"real registry", false},
	} {
		p, err := BuildPointer(PointerInput{
			Env: "dev-k8s", Repository: "reg/p", Digest: validDigest(t, testDigest),
			Cluster: "k3d-a", Insecure: tc.insecure,
			ClusterPaths: []release.BundleClusterTree{
				{Cluster: "k3d-a", Path: release.BundleClusterPath("k3d-a"), Documents: 1},
			},
		})
		if err != nil {
			t.Fatalf("%s: BuildPointer: %v", tc.name, err)
		}
		_, present := p.Source.Doc["spec"].(map[string]any)["insecure"]
		if present != tc.insecure {
			t.Errorf("%s: insecure present = %v, want %v — a false that is written puts the word "+
				"\"insecure\" on every real registry's pointer for a reader to misread", tc.name, present, tc.insecure)
		}
	}
}

// TestBuildPointer_RefusesWhatItCannotGuess. Every field refused here would
// otherwise have to be defaulted, and each default applies bytes nobody
// recorded while reporting success.
func TestBuildPointer_RefusesWhatItCannotGuess(t *testing.T) {
	t.Parallel()
	paths := []release.BundleClusterTree{{Cluster: "k3d-a", Path: "manifests/k3d-a", Documents: 1}}
	for _, tc := range []struct {
		name string
		in   PointerInput
		want string
	}{
		{"no env", PointerInput{Repository: "r", Digest: testDigest, Cluster: "k3d-a", ClusterPaths: paths}, "needs an env"},
		{"no repository", PointerInput{Env: "e", Digest: testDigest, Cluster: "k3d-a", ClusterPaths: paths}, "repository"},
		{"no cluster", PointerInput{Env: "e", Repository: "r", Digest: testDigest, ClusterPaths: paths}, "cluster"},
		{"tag, not digest", PointerInput{Env: "e", Repository: "r", Digest: "v1.2.0", Cluster: "k3d-a", ClusterPaths: paths}, "canonical"},
		{"short digest", PointerInput{Env: "e", Repository: "r", Digest: "sha256:abc", Cluster: "k3d-a", ClusterPaths: paths}, "canonical"},
	} {
		_, err := BuildPointer(tc.in)
		if err == nil {
			t.Errorf("%s: BuildPointer succeeded; it must refuse rather than guess", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
}

// TestBuildPointer_EmptyWhenNothingRoutesHere pins that a cluster with no path
// produces a reportable EMPTY pointer rather than a source nothing consumes.
func TestBuildPointer_EmptyWhenNothingRoutesHere(t *testing.T) {
	t.Parallel()
	p, err := BuildPointer(PointerInput{
		Env: "dev-k8s", Repository: "k3d-reg:5000/p", Digest: validDigest(t, testDigest),
		Cluster:      "k3d-nothing-here",
		ClusterPaths: []release.BundleClusterTree{{Cluster: "k3d-a", Path: "manifests/k3d-a", Documents: 4}},
	})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	if !p.Empty() {
		t.Fatalf("Empty() = false with %d kustomizations; a cluster the render routed nothing to has nothing to apply",
			len(p.Kustomizations))
	}
}

// TestPointerAll_SourceFirst pins the apply order. A Kustomization applied
// before the OCIRepository it names reports `source not found` on the first
// poll, which makes a healthy deploy's first observation a failure reason that
// is not the truth.
func TestPointerAll_SourceFirst(t *testing.T) {
	t.Parallel()
	p, err := BuildPointer(PointerInput{
		Env: "dev-k8s", Repository: "k3d-reg:5000/p", Digest: validDigest(t, testDigest), Cluster: "k3d-a",
		ClusterPaths: []release.BundleClusterTree{{Cluster: "k3d-a", Path: "manifests/k3d-a", Documents: 1}},
	})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	all := p.All()
	if len(all) != 2 || all[0].Kind != kindOCIRepository {
		t.Fatalf("All() kinds = %s,%s; the source must come first", all[0].Kind, all[len(all)-1].Kind)
	}
}

// TestKustomizationName_CarriesTheClusterOnlyWhenMulti pins the naming: a
// single-cluster env reads as its own, and a multi-cluster env's two paths
// cannot collide on one name (which would write one object twice and silently
// keep the last).
func TestKustomizationName_CarriesTheClusterOnlyWhenMulti(t *testing.T) {
	t.Parallel()
	if got := KustomizationName("dev-k8s", ""); got != "dev-k8s" {
		t.Errorf("unclustered name = %q, want dev-k8s", got)
	}
	a := KustomizationName("dev-k8s", "k3d-control-plane")
	b := KustomizationName("dev-k8s", "k3d-cp-daemon")
	if a == b {
		t.Fatalf("two clusters produced one name %q; the second path would overwrite the first", a)
	}
	// RFC-1123: a kubectl context is free-form, an object name is not.
	for _, n := range []string{a, b, KustomizationName("dev_k8s", "gke_proj_us-central1_prod")} {
		if strings.ContainsAny(n, "_ ABCDEFGHIJKLMNOPQRSTUVWXYZ") || strings.HasPrefix(n, "-") || strings.HasSuffix(n, "-") {
			t.Errorf("name %q is not RFC-1123 safe; a kubectl context can contain anything", n)
		}
	}
}

// TestInsecureRegistry pins the predicate that decides a plaintext fetch. The
// safety property is that only addresses no certificate could cover can
// produce one.
func TestInsecureRegistry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{"localhost:5050/p", true},
		{"127.0.0.1:5050/p", true},
		{"registry.localhost:5000/p", true},
		{"dev.localhost:5050/p", true},
		{"host.k3d.internal:5050/p", true},
		// A k3d registry's in-network name is its container name.
		{"k3d-dev-registry:5000/p", true},
		{"oci://k3d-dev-registry:5000/p", true},
		// Real registries must never be downgraded.
		{"ghcr.io/reliant-labs/p", false},
		{"us-central1-docker.pkg.dev/proj/repo/p", false},
		{"123.dkr.ecr.us-east-1.amazonaws.com/p", false},
		// Not a k3d registry just because the PATH says so.
		{"ghcr.io/k3d-dev-registry/p", false},
	} {
		if got := InsecureRegistry(tc.ref); got != tc.want {
			t.Errorf("InsecureRegistry(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

// TestChartProvidedBy_ByReferenceNotName is the SHARED-FLUX RULE.
//
// Two Flux installs in one cluster collide on the same cluster-scoped CRDs and
// the same `flux-system`, and the collision is not clean: whichever applies
// last owns the CRDs and the other's Kustomizations reconcile against a
// controller replaced underneath them. Detection must therefore key off the
// CHART, not the chart's freely-chosen `name`.
func TestChartProvidedBy_ByReferenceNotName(t *testing.T) {
	t.Parallel()
	if !ChartProvidedBy(ChartOCI) {
		t.Fatal("the pinned chart reference is not recognised as providing Flux")
	}
	// Spellings of one reference.
	for _, ref := range []string{
		"ghcr.io/fluxcd-community/charts/flux2",
		"OCI://ghcr.io/fluxcd-community/charts/flux2",
		"oci://ghcr.io/fluxcd-community/charts/flux2/",
	} {
		if !ChartProvidedBy(ref) {
			t.Errorf("ChartProvidedBy(%q) = false; it is the same chart under a different spelling", ref)
		}
	}
	// A DIFFERENT chart does not provide Flux, however it is named — this
	// is the half that would be wrong under a name heuristic.
	for _, ref := range []string{
		"oci://docker.io/envoyproxy/gateway-helm",
		"https://charts.jetstack.io",
		"oci://ghcr.io/some-fork/charts/flux2",
		"",
	} {
		if ChartProvidedBy(ref) {
			t.Errorf("ChartProvidedBy(%q) = true; only the pinned chart provides Flux", ref)
		}
	}
}
