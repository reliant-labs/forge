package bundle

import (
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/pkg/release"
)

// ShapeInput is one env's render, as the projection needs it.
//
// The caller supplies two kinds of thing, and the split is deliberate. The
// DECLARATION half (kind, workloads, secrets, domains, clusters) is read off
// the rendered entity contract, which only internal/cli can decode. The
// MANIFEST half is the rendered stream itself, which this package parses, so
// the bytes that get hashed are the bytes a deploy applies — not a second
// model of them.
type ShapeInput struct {
	// Kind is the env kind the render implies (forge's hostedEnvKindOf
	// predicate, in pkg/release vocabulary). Required.
	Kind release.EnvKind
	// Workloads, Secrets, Domains and Clusters are the declaration's own
	// projection, already in shape vocabulary. Secrets carry NAMES.
	Workloads []release.ShapeWorkload
	Secrets   []release.ShapeSecret
	Domains   []string
	Clusters  []string

	// Manifests is the env's rendered manifest stream, exactly as
	// `forge env render <env>` prints it: `---`-separated documents, each
	// optionally preceded by the `# cluster: <a>, <b>` comment naming the
	// cluster(s) a deploy routes it to. A document with no such comment,
	// or one naming none, is attributed to no cluster rather than to a
	// guessed one.
	Manifests string

	// Images maps each release artifact key to the digest this render
	// pinned it to. It is what makes [release.ShapeObject.Images] and
	// ConfigHash possible: an object's images are the pins that appear in
	// it, and its ConfigHash is its hash with those pins normalized away.
	Images map[string]string

	// StatefulWorkloads names the workloads whose objects hold data beyond
	// their own spec — in practice the env's declared ManagedDatabases,
	// whose records expand into a Postgres cluster, its Services and its
	// volumes. The well-known stateful kinds (PVC, StatefulSet, Secret, …)
	// are recognised by pkg/release without being listed here; this is for
	// the objects whose kind alone does not say so.
	StatefulWorkloads []string
}

// ProjectShape projects one env's render into the shape a ledger indexes.
//
// It is the ONE projection: `forge env shape`, the declaration `forge env
// build` records, and the bundle F2 writes all call this, so a shape shown to
// a user, a shape stored on an env and a shape sealed inside a bundle cannot
// describe the render differently.
//
// NO SECRET VALUE SURVIVES IT. Every `kind: Secret` document has its
// `data` / `stringData` values replaced by the hash of the value BEFORE the
// document is hashed, so neither the shape nor any hash input carries one
// (F-13). The hash still moves when a value moves, which is what keeps
// "a secret changed" visible without the secret.
func ProjectShape(in ShapeInput) (release.Shape, error) {
	if !in.Kind.Valid() {
		return release.Shape{}, fmt.Errorf("%w: shape input: environment kind %q", release.ErrInvalid, in.Kind)
	}
	objects, err := projectObjects(in)
	if err != nil {
		return release.Shape{}, err
	}
	shape := release.Shape{
		Kind:      in.Kind,
		Workloads: in.Workloads,
		Secrets:   in.Secrets,
		Domains:   in.Domains,
		Clusters:  in.Clusters,
		Objects:   objects,
	}.Canonical()
	if err := shape.Validate(); err != nil {
		return release.Shape{}, err
	}
	return shape, nil
}

// projectObjects turns the manifest stream into the shape's objects, through
// the SAME parse [Build] packages from — see parse.go on why that matters.
func projectObjects(in ShapeInput) ([]release.ShapeObject, error) {
	docs, err := parseStream(in.Manifests)
	if err != nil {
		return nil, fmt.Errorf("shape: %w", err)
	}
	objects, err := shapeObjects(docs, in.Images, in.StatefulWorkloads)
	if err != nil {
		return nil, fmt.Errorf("shape: %w", err)
	}
	return objects, nil
}

// ─── The stream ──────────────────────────────────────────────────────────────

// clusterComment is the per-document routing header `forge env render` emits.
const clusterComment = "# cluster:"

// noClustersDeclared is what that header carries for a document the deploy
// layer routes nowhere, and it must not be read as a cluster NAMED
// "(none declared)".
const noClustersDeclared = "(none declared)"

type streamDoc struct {
	yaml     string
	clusters []string
}

// splitStream splits the annotated manifest stream into documents, reading
// each one's `# cluster:` header off the front.
//
// Splitting on a lone `---` line rather than reusing internal/cluster's
// splitter keeps this package free of the deploy machinery (kubectl, KCL,
// helm), which is what lets the projection be tested from a literal fixture.
// It inherits that splitter's one limitation — a `---` inside a block scalar
// would split a document — and the stream forge renders contains none.
func splitStream(stream string) []streamDoc {
	var docs []streamDoc
	current := streamDoc{}
	var body []string
	flush := func() {
		if strings.TrimSpace(strings.Join(body, "\n")) != "" {
			current.yaml = strings.Join(body, "\n")
			docs = append(docs, current)
		}
		current, body = streamDoc{}, nil
	}
	for _, line := range strings.Split(stream, "\n") {
		switch {
		case strings.TrimSpace(line) == "---":
			flush()
		case strings.HasPrefix(strings.TrimSpace(line), clusterComment) && len(body) == 0:
			current.clusters = parseClusters(strings.TrimSpace(line))
		default:
			body = append(body, line)
		}
	}
	flush()
	return docs
}

func parseClusters(line string) []string {
	rest := strings.TrimSpace(strings.TrimPrefix(line, clusterComment))
	if rest == "" || rest == noClustersDeclared {
		return nil
	}
	var out []string
	for _, name := range strings.Split(rest, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// ─── Identity ───────────────────────────────────────────────────────────────

// Annotation keys that pin an object to a reserved external address. Closed,
// because [release.IdentityKeys] is closed: the identity map exists to carry
// the few attributes a deploy can silently take away, and an open map would
// become somewhere a value hides.
const (
	gkeGlobalStaticIPAnnotation   = "kubernetes.io/ingress.global-static-ip-name"
	gkeRegionalStaticIPAnnotation = "kubernetes.io/ingress.regional-static-ip-name"
	gkeGatewayStaticIPAnnotation  = "networking.gke.io/static-ip"
)

// identityOf reads the attributes that give an object an EXTERNAL identity —
// an address the world already points at, which a deploy can release without
// looking like it changed anything.
//
// Three kinds carry one in a forge render:
//
//   - a Service, through its type and load-balancer fields. Moving a
//     LoadBalancer Service to ClusterIP frees its address, and the next
//     deploy that re-creates it gets a different one;
//   - an Ingress or Gateway pinned to a reserved address by annotation;
//   - an EnvoyProxy, which is where forge puts a Gateway's IPAddress pin
//     (`provider.kubernetes.envoyService.loadBalancerIP`) — see
//     kcl/lib/gateway.k on why the pin cannot ride spec.addresses.
//
// Everything else returns nil. An object with no external identity must not
// carry an empty map: nil and "{}" would read alike to a diff and the plan's
// stop rules key on presence.
func identityOf(kind string, body any) map[string]string {
	doc, ok := body.(map[string]any)
	if !ok {
		return nil
	}
	id := map[string]string{}
	switch kind {
	case "Service":
		spec, _ := doc["spec"].(map[string]any)
		put(id, release.IdentityType, stringAt(spec, "type"))
		put(id, release.IdentityLoadBalancerIP, stringAt(spec, "loadBalancerIP"))
		put(id, release.IdentityLoadBalancerClass, stringAt(spec, "loadBalancerClass"))
	case "EnvoyProxy":
		put(id, release.IdentityLoadBalancerIP, stringAt(
			mapAt(mapAt(mapAt(doc, "spec"), "provider"), "kubernetes"), "envoyService", "loadBalancerIP"))
	case "Ingress", "Gateway":
		annotations := mapAt(mapAt(doc, "metadata"), "annotations")
		for _, key := range []string{gkeGlobalStaticIPAnnotation, gkeRegionalStaticIPAnnotation, gkeGatewayStaticIPAnnotation} {
			put(id, release.IdentityStaticIP, stringAt(annotations, key))
		}
	}
	if len(id) == 0 {
		return nil
	}
	return id
}

// put records a non-empty value, and never overwrites one already recorded:
// the first annotation of a closed key wins, so two spellings of "pin this to
// a reserved address" cannot make the identity depend on map iteration order.
func put(id map[string]string, key, value string) {
	if value != "" && id[key] == "" {
		id[key] = value
	}
}

// ─── Decoding helpers ───────────────────────────────────────────────────────

type docMeta struct {
	apiVersion string
	kind       string
	namespace  string
	name       string
	workload   string
}

// forgeWorkloadLabel is forge's routing key — the deploy group an object
// belongs to (internal/cluster.WorkloadLabel). Spelled here rather than
// imported so this package does not depend on the deploy machinery; the
// value is forge's own and is pinned by kcl/tests.
const forgeWorkloadLabel = "forge.dev/workload"

// appNameLabel is the Kubernetes-standard owner label, which forge stamps on
// everything it builds and which stands in when the routing key is absent.
const appNameLabel = "app.kubernetes.io/name"

func readMeta(body any) docMeta {
	doc, _ := body.(map[string]any)
	metadata := mapAt(doc, "metadata")
	labels := mapAt(metadata, "labels")
	workload := stringAt(labels, forgeWorkloadLabel)
	if workload == "" {
		workload = stringAt(labels, appNameLabel)
	}
	return docMeta{
		apiVersion: stringAt(doc, "apiVersion"),
		kind:       stringAt(doc, "kind"),
		namespace:  stringAt(metadata, "namespace"),
		name:       stringAt(metadata, "name"),
		workload:   workload,
	}
}

// mapAt walks one level into a decoded document, returning nil rather than
// failing: a manifest is hostile input as far as this package is concerned,
// and a missing nesting level is an absent field, not an error.
func mapAt(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	nested, _ := m[key].(map[string]any)
	return nested
}

// stringAt reads a string at a path, walking intermediate maps.
func stringAt(m map[string]any, path ...string) string {
	for len(path) > 1 {
		m = mapAt(m, path[0])
		path = path[1:]
	}
	if m == nil || len(path) == 0 {
		return ""
	}
	s, _ := m[path[0]].(string)
	return s
}
