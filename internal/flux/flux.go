// Package flux builds the DESIRED-STATE POINTER forge writes into a cluster
// that reconciles itself, and reads the convergence back out.
//
// THE MODEL. Every real environment goes bundle → version store → reconciler.
// An env with no control plane uses the machine ledger as its version store,
// and the reconciler — Flux — runs IN its cluster. forge's whole job on that
// path is to write two small objects per cluster:
//
//	OCIRepository   flux-system/<env>   "the bundle is at this digest"
//	Kustomization   flux-system/<env>[-<cluster>]   "apply this path from it"
//
// and then watch. It applies none of the env's own objects. That is the point:
// the thing that writes the pointer and the thing that converges the cluster
// are different, so a half-finished forge run cannot leave a cluster half
// applied, and the cluster re-converges after a node replacement with no forge
// present at all.
//
// THE POINTER IS BY DIGEST, NEVER BY TAG. `ref.digest` names immutable bytes,
// so the Kustomization's `lastAppliedRevision` can be compared string-equal
// against the digest forge recorded — which is the ENTIRE basis of the wait.
// A tag would make "converged" mean "converged to whatever that tag pointed at
// when source-controller last looked", and a wait on it would pass for a
// release it never saw.
//
// WHY `lastAppliedRevision` EQUALS THE OCI MANIFEST DIGEST. source-controller
// sets an OCIRepository artifact's revision from the digest it RESOLVED, and
// kustomize-controller copies that revision into `lastAppliedRevision` when an
// apply succeeds. `layerSelector` selects WHICH layer is extracted; it does not
// re-key the revision off the layer's own digest. So the revision is the
// manifest digest even though what was applied came out of one layer. That is
// reasoned from the controllers' behaviour rather than asserted here, which is
// why [WaitConverged]'s contract is pinned by an e2e that reads a real
// Kustomization rather than by a unit test alone: if a future source-controller
// changed it, every wait on this path would hang on a comparison that can never
// be true, and a unit test over forge's own structs could not tell.
//
// EVERYTHING IN THIS FILE IS PURE. Building the objects reads no cluster and
// no clock it did not receive, so the pointer for a given (env, digest,
// cluster) is a value a test can state exactly. The kubectl shell-outs live in
// apply.go and wait.go.
package flux

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// The chart forge installs, and the one a consumer declares to be recognised
// as already providing Flux.
//
// THESE TWO STRINGS ARE SHARED WITH KCL and must not drift:
// `kcl/lib/flux.k`'s FLUX_CHART_OCI / FLUX_CHART_VERSION are the same values,
// and kcl/tests/positive_flux_chart.k pins them against the literals. The
// reference is the SHARED-FLUX DETECTOR — [ChartProvidedBy] compares a
// declared chart's `oci` against ChartOCI — so a divergence here would make a
// consumer's Flux undetectable and forge would install a second one into a
// cluster that already had one.
const (
	// ChartOCI is the community flux2 chart. There is no fluxcd-published
	// chart; this is the canonical install path.
	ChartOCI = "oci://ghcr.io/fluxcd-community/charts/flux2"
	// ChartVersion is the pinned CHART version. Chart 2.19.1 is appVersion
	// v2.9.5 (source-controller and kustomize-controller v1.9.5). The two
	// numbers are unrelated lineages — re-derive with
	// `helm show chart <ChartOCI> --version <v> | grep appVersion` on a
	// bump, never increment.
	ChartVersion = "2.19.1"
	// ChartName is the `--target` selector forge's own install uses. A
	// consumer may name its chart anything, which is exactly why detection
	// is by reference and not by this.
	ChartName = "flux"
	// Namespace is Flux's namespace, and where the pointer is written. Not
	// configurable: a second spelling would only let the writer and the
	// controller disagree about where the OCIRepository lives.
	Namespace = "flux-system"
)

// The API versions the pointer is written against. Pinned to the versions
// shipped by the chart above (source.toolkit v1 and kustomize.toolkit v1 are
// both GA in Flux v2.9.5), rather than to `v1beta2`: an object written at a
// beta version a future chart drops would fail to apply with a kind error that
// names the version and not the cause.
const (
	apiVersionSource    = "source.toolkit.fluxcd.io/v1"
	apiVersionKustomize = "kustomize.toolkit.fluxcd.io/v1"

	kindOCIRepository = "OCIRepository"
	kindKustomization = "Kustomization"
)

// FieldManager is the server-side-apply field manager every write on this path
// uses.
//
// ONE MANAGER, NAMED FOR FORGE, because SSA's conflict detection is per
// manager: a second spelling would make forge's own next write conflict with
// its own previous one. It is also the audit answer to "who set this field" on
// an object a human may also have touched — a `kubectl apply` of the same
// object arrives as `kubectl-client-side-apply` and is visibly not forge.
const FieldManager = "forge"

// Annotations forge sets on the pointer.
const (
	// AnnotationRequestedAt asks Flux to reconcile NOW rather than at the
	// next interval. Its VALUE is what matters, not its presence: the
	// controllers re-reconcile when it CHANGES, so it carries the write's
	// timestamp. A constant would be applied once and then be a no-op
	// forever, which would make every deploy after the first wait out the
	// poll interval for no reason.
	AnnotationRequestedAt = "reconcile.fluxcd.io/requestedAt"
	// AnnotationPruneDisabled exempts one object from the Kustomization's
	// prune. Stamped at BUNDLE WRITE TIME (internal/bundle) rather than
	// here, so every reconciler that ever applies those bytes honours it —
	// including one forge did not write the pointer for.
	AnnotationPruneDisabled = "kustomize.toolkit.fluxcd.io/prune"
	// PruneDisabled is AnnotationPruneDisabled's value.
	PruneDisabled = "disabled"
)

// Labels forge sets on the pointer, so the objects it owns are identifiable
// without reading their names.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelEnv       = "forge.dev/env"
	// LabelCluster is the kubectl context a Kustomization's path is routed
	// to. Present on a multi-cluster env's per-cluster Kustomizations and
	// absent on the unclustered one.
	LabelCluster = "forge.dev/cluster"
)

// PointerInput is everything the pointer for ONE cluster is a function of.
//
// It is deliberately not "the env's render": the pointer says WHERE THE BYTES
// ARE and WHICH PATH to apply, and neither is a property of the render. A
// writer that accepted a render here could build a pointer whose path did not
// exist in the artifact, which is the one failure that reconciles GREEN over an
// env nothing applied.
type PointerInput struct {
	// Env is the environment. It names the OCIRepository and prefixes the
	// Kustomizations, so one cluster can host several envs' pointers.
	Env string
	// Repository is the bundle's repository, addressed as THE CLUSTER CAN
	// REACH IT — for a k3d cluster that is the registry's in-network name
	// (`k3d-<name>-registry:5000/...`), not the `localhost:<port>` the host
	// pushes to. Those are the same registry under two names, and writing
	// the host's name here produces an OCIRepository that fails to fetch
	// from inside the node with a connection refused that names localhost.
	Repository string
	// Digest is the bundle's OCI MANIFEST digest, canonical
	// `sha256:<64 hex>`. This is both what `ref.digest` pins and what
	// `lastAppliedRevision` is compared against.
	Digest string
	// Cluster is the kubectl context this pointer is written INTO. Only
	// the ClusterPaths entries routed to it get a Kustomization: a path
	// belonging to another cluster, applied here, would apply that
	// cluster's objects into this one.
	Cluster string
	// ClusterPaths is [release.BundleDoc.ClusterPaths] — the paths the
	// layer ACTUALLY HOLDS. Never the env's declared cluster list: a
	// declared cluster with no documents routed to it has no path in the
	// artifact, and a Kustomization pointing at one reconciles green over
	// nothing.
	ClusterPaths []release.BundleClusterTree
	// Insecure allows plain HTTP to the registry. True ONLY for a local
	// k3d registry, which is served over HTTP and has no certificate any
	// cluster could verify. [InsecureRegistry] is the predicate; a caller
	// that set this from a flag would be able to downgrade a real
	// registry's transport.
	Insecure bool
	// RequestedAt is the timestamp for AnnotationRequestedAt. The
	// caller's, because this package reads no clock: the pointer for a
	// given input must be a value a test can state, and a function that
	// read `time.Now` could not be compared byte for byte.
	RequestedAt time.Time
}

// The pointer's two cadences. They answer different questions and are
// deliberately not one number.
const (
	// DefaultInterval is how often Flux re-fetches and re-applies with no
	// prompting. It is the drift-correction loop: a hand-edited field is
	// reverted within it, and a deleted object is recreated. One minute is
	// short enough that a human experimenting sees their edit reverted
	// while still watching, and long enough that a registry is not polled
	// pointlessly — and because the pointer carries `requestedAt`, a
	// DEPLOY never waits for it.
	DefaultInterval = time.Minute
	// DefaultApplyTimeout bounds one apply-and-health-check attempt
	// INSIDE Flux. It must be shorter than forge's own wait budget, or a
	// wedged apply would exhaust forge's budget without Flux ever
	// reporting the failure that caused it — forge would report a timeout
	// where Flux knew the reason.
	DefaultApplyTimeout = 5 * time.Minute
)

// Object is one rendered pointer object: the document to apply, plus the
// identity the wait needs to find it again.
//
// The document is `map[string]any` rather than a typed struct because forge
// does not depend on Flux's Go API — pulling fluxcd's module in for two object
// shapes would add a transitive dependency tree an order of magnitude larger
// than the code it serves, and pin forge's build to Flux's own Kubernetes
// version. The shapes are small, closed and pinned by test.
type Object struct {
	Kind      string
	Name      string
	Namespace string
	// Cluster is the kubectl context this object is written INTO.
	Cluster string
	// Doc is the object, ready to marshal.
	Doc map[string]any
}

// Pointer is one cluster's complete desired-state pointer.
type Pointer struct {
	// Source is the OCIRepository: where the bytes are.
	Source Object
	// Kustomizations is one per ClusterPaths entry routed to this
	// cluster. Possibly EMPTY, and that is a real state a caller must
	// handle rather than a failure: a cluster the env declares but routes
	// no documents to has nothing to apply there. [Pointer.Empty] says so.
	Kustomizations []Object
}

// Empty reports that this cluster has no path to apply.
//
// A caller must not treat it as success-with-nothing-to-do silently: the env
// declared this cluster, so an empty pointer means the render routed no
// document to a cluster someone expected objects on. The deploy path says so
// and writes nothing, rather than applying a source nothing consumes.
func (p Pointer) Empty() bool { return len(p.Kustomizations) == 0 }

// All is every object in the pointer, source first.
//
// ORDER IS LOAD-BEARING: a Kustomization applied before the OCIRepository it
// names is admitted and then reports `source not found` until the source
// arrives. Harmless but noisy, and it makes the first poll of a fresh deploy
// report a failure reason that is not the truth.
func (p Pointer) All() []Object {
	out := make([]Object, 0, len(p.Kustomizations)+1)
	out = append(out, p.Source)
	return append(out, p.Kustomizations...)
}

// BuildPointer renders one cluster's pointer.
//
// It REFUSES rather than defaults on a malformed input, because every field it
// would have to guess at produces a pointer that reconciles green over the
// wrong thing: a missing digest would have to become a tag, a missing
// repository would have to become a default registry, and either one applies
// bytes nobody recorded while reporting success.
func BuildPointer(in PointerInput) (Pointer, error) {
	env := strings.TrimSpace(in.Env)
	switch {
	case env == "":
		return Pointer{}, fmt.Errorf("%w: flux pointer needs an env", release.ErrInvalid)
	case strings.TrimSpace(in.Repository) == "":
		return Pointer{}, fmt.Errorf("%w: flux pointer for env %s needs the bundle's repository, "+
			"addressed as the cluster can reach it", release.ErrInvalid, env)
	case !release.ValidDigest(in.Digest):
		return Pointer{}, fmt.Errorf("%w: flux pointer for env %s pins %q, which is not a canonical "+
			"sha256 digest — a pointer is by digest so that lastAppliedRevision can be compared to it",
			release.ErrInvalid, env, in.Digest)
	case strings.TrimSpace(in.Cluster) == "":
		return Pointer{}, fmt.Errorf("%w: flux pointer for env %s needs the cluster it is written into",
			release.ErrInvalid, env)
	}

	interval, timeout := DefaultInterval, DefaultApplyTimeout
	sourceName := SourceName(env)

	out := Pointer{Source: Object{
		Kind: kindOCIRepository, Name: sourceName, Namespace: Namespace, Cluster: in.Cluster,
		Doc: map[string]any{
			"apiVersion": apiVersionSource,
			"kind":       kindOCIRepository,
			"metadata": map[string]any{
				"name":        sourceName,
				"namespace":   Namespace,
				"labels":      labelsFor(env, ""),
				"annotations": map[string]any{AnnotationRequestedAt: requestedAt(in.RequestedAt)},
			},
			"spec": sourceSpec(in, interval, timeout),
		},
	}}

	for _, tree := range routedTo(in.ClusterPaths, in.Cluster) {
		name := KustomizationName(env, tree.Cluster)
		out.Kustomizations = append(out.Kustomizations, Object{
			Kind: kindKustomization, Name: name, Namespace: Namespace, Cluster: in.Cluster,
			Doc: map[string]any{
				"apiVersion": apiVersionKustomize,
				"kind":       kindKustomization,
				"metadata": map[string]any{
					"name":        name,
					"namespace":   Namespace,
					"labels":      labelsFor(env, tree.Cluster),
					"annotations": map[string]any{AnnotationRequestedAt: requestedAt(in.RequestedAt)},
				},
				"spec": kustomizationSpec(sourceName, tree.Path, interval, timeout),
			},
		})
	}
	return out, nil
}

// sourceSpec is the OCIRepository's spec.
//
// `layerSelector` IS THE CONTRACT WITH THE BUNDLE. A bundle is one layer of
// [release.BundleManifestsLayer], and `operation: extract` untars it into the
// artifact so a Kustomization can apply the YAML under a path inside it.
// Without the selector, source-controller would hand kustomize-controller the
// tarball itself and the Kustomization's path would not exist — which
// reconciles as "nothing to apply" and reports Ready.
func sourceSpec(in PointerInput, interval, timeout time.Duration) map[string]any {
	spec := map[string]any{
		"url":      "oci://" + strings.TrimPrefix(strings.TrimSpace(in.Repository), "oci://"),
		"interval": duration(interval),
		"timeout":  duration(timeout),
		"ref":      map[string]any{"digest": in.Digest},
		"layerSelector": map[string]any{
			"mediaType": release.BundleManifestsLayer,
			"operation": "extract",
		},
	}
	// `insecure` is written ONLY when true. A false that is present and a
	// false that is absent mean the same thing to Flux, and writing it
	// would put the word "insecure" on every real registry's pointer for a
	// reader to misread.
	if in.Insecure {
		spec["insecure"] = true
	}
	return spec
}

// kustomizationSpec is one path's Kustomization.
//
// `prune: true` is what makes the bundle AUTHORITATIVE: an object the path no
// longer carries is deleted, so a removed workload actually goes away instead
// of outliving the render that declared it. It is also why the stateful
// objects carry `kustomize.toolkit.fluxcd.io/prune: disabled` from bundle
// write time — a prune that could delete a PVC on a render that merely failed
// to select it is the one blast radius this path must not have.
//
// `wait: true` makes Flux health-check what it applied, so Ready=True means
// the Deployments rolled out rather than that the YAML was accepted. Without
// it forge's wait would pass the moment the apply was admitted, which is
// before any new pod has served.
//
// NO `targetNamespace`. The render already stamps every namespaced object's
// `metadata.namespace`, so setting one would OVERRIDE the render for every
// object at once — silently relocating anything the env deliberately placed
// elsewhere (a chart's namespace, kube-system). The brief allows it "only if
// the render requires it", and the render never does: forge's own renderer
// emits the Namespace object and stamps its members.
func kustomizationSpec(sourceName, path string, interval, timeout time.Duration) map[string]any {
	return map[string]any{
		"interval":      duration(interval),
		"timeout":       duration(timeout),
		"retryInterval": duration(interval),
		"path":          "./" + strings.TrimPrefix(path, "./"),
		"prune":         true,
		"wait":          true,
		"sourceRef": map[string]any{
			"kind":      kindOCIRepository,
			"name":      sourceName,
			"namespace": Namespace,
		},
	}
}

// routedTo is the ClusterPaths entries this cluster applies.
//
// Only an EXACT cluster match, and the unclustered tree is deliberately
// excluded. The unclustered path holds documents the deploy layer attributes
// to no cluster — a host-only env's objects, which nothing applies — so a
// Kustomization over it would apply objects into a cluster their declaration
// never named. An empty-path entry is skipped too: `Documents == 0` means the
// tree exists in the doc but holds nothing, and a Kustomization over it
// reports Ready having applied nothing.
func routedTo(trees []release.BundleClusterTree, cluster string) []release.BundleClusterTree {
	want := strings.TrimSpace(cluster)
	var out []release.BundleClusterTree
	for _, t := range trees {
		if strings.TrimSpace(t.Cluster) != want || want == "" {
			continue
		}
		if t.Documents <= 0 || strings.TrimSpace(t.Path) == "" {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// SourceName is the OCIRepository's name: the env.
//
// Named for the ENV and not for the bundle, because it is the env's pointer
// and it is UPDATED in place on every deploy. A name carrying the digest would
// leave one dead OCIRepository per release in `flux-system` forever, and
// nothing would collect them.
func SourceName(env string) string { return k8sName(env) }

// KustomizationName is one path's Kustomization: the env, plus the cluster
// when an env spans several.
//
// A single-cluster env gets a bare `<env>`, which is the common case and reads
// as the env's own Kustomization. A multi-cluster env's name carries the
// cluster so two paths' Kustomizations cannot collide — they land in different
// clusters today, but a cluster that hosts two of an env's paths (an env
// declaring the same context twice under different names) would otherwise
// write one name twice and silently keep only the last.
func KustomizationName(env, cluster string) string {
	if strings.TrimSpace(cluster) == "" {
		return k8sName(env)
	}
	return k8sName(env + "-" + cluster)
}

// k8sName makes a name RFC-1123 safe: lowercase alphanumerics and dashes.
//
// A kubectl CONTEXT is free-form (`gke_project_region_name` is the tame case)
// while an object name is not, so the sanitization has to happen somewhere.
// Here, once, rather than at each call site — two spellings would name two
// objects and the wait would poll the one that was not written.
func k8sName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	// A name may not start or end with a dash, and may not be empty. The
	// callers have already refused an empty env, so this is the
	// all-punctuation case.
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "forge"
	}
	return out
}

func labelsFor(env, cluster string) map[string]any {
	out := map[string]any{
		LabelManagedBy: FieldManager,
		LabelEnv:       k8sName(env),
	}
	if strings.TrimSpace(cluster) != "" {
		out[LabelCluster] = k8sName(cluster)
	}
	return out
}

// requestedAt is the annotation's value: RFC 3339, to the second.
//
// A zero time would write an empty annotation, which Flux reads as "no request
// made" — so the write would apply and then sit until the interval elapsed,
// making every deploy's first minute look like a hang. The caller's clock is
// used when it gave one; a zero one falls back to a value that still CHANGES
// relative to whatever is on the object, which is what triggers the
// reconcile.
func requestedAt(at time.Time) string {
	if at.IsZero() {
		at = time.Now()
	}
	return at.UTC().Format(time.RFC3339)
}

// duration formats a Go duration as the Kubernetes duration string Flux
// parses. Seconds, because that is the resolution these cadences need and it
// round-trips through metav1.Duration unambiguously.
func duration(d time.Duration) string {
	if d < time.Second {
		d = time.Second
	}
	return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
}

// InsecureRegistry reports whether a registry reference is served over plain
// HTTP: the loopback and `*.localhost` names a local k3d registry is published
// under, and the in-network `k3d-…-registry:5000` container name pods pull by.
//
// It is a PREDICATE OVER THE ADDRESS rather than a flag, which is the whole
// safety property: `insecure: true` downgrades the transport, so a caller able
// to pass it in could point a cloud env's pointer at a plaintext fetch. Here
// the only addresses that can produce it are ones no certificate could cover
// anyway.
func InsecureRegistry(ref string) bool {
	host, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(ref), "oci://"), "/")
	hostOnly, _, _ := strings.Cut(host, ":")
	switch {
	case hostOnly == "localhost", hostOnly == "127.0.0.1", hostOnly == "registry.localhost":
		return true
	case strings.HasSuffix(hostOnly, ".localhost"):
		return true
	case hostOnly == "host.k3d.internal":
		return true
	// A k3d registry's in-network name is its CONTAINER name, which k3d
	// always prefixes `k3d-`. Pods reach it by that name on the cluster's
	// docker network, over HTTP, with no certificate for it in existence.
	case strings.HasPrefix(hostOnly, "k3d-"):
		return true
	}
	return false
}

// ChartProvidedBy reports whether any of the chart references given already
// provides Flux — the SHARED-FLUX RULE.
//
// DETECTION IS BY CHART REFERENCE, NEVER BY NAME, and the distinction is what
// keeps this correct. A HelmChart's `name` is the `--target` selector its
// author picks freely: control-plane calls its hosted Flux "flux", another
// consumer might call it "flux-system" or "gitops", and a name heuristic would
// then decide whether forge installed a SECOND Flux into a cluster that
// already had one. Two Flux installs in one cluster collide on the same
// cluster-scoped CRDs and the same `flux-system` namespace, and the collision
// is not a clean failure: whichever applied last owns the CRDs, and the
// other's Kustomizations reconcile against a controller that was replaced
// underneath them.
//
// The comparison ignores an `oci://` prefix and a trailing slash, because
// those are spellings of one reference rather than different charts.
func ChartProvidedBy(refs ...string) bool {
	for _, ref := range refs {
		if sameChartRef(ref, ChartOCI) {
			return true
		}
	}
	return false
}

func sameChartRef(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(strings.ToLower(s))
		s = strings.TrimPrefix(s, "oci://")
		return strings.TrimSuffix(s, "/")
	}
	a, b = norm(a), norm(b)
	return a != "" && a == b
}
