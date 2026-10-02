package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

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

// projectObjects turns the manifest stream into one [release.ShapeObject] per
// (document, cluster) pair.
//
// PER PAIR, not per document: an unattributed env-level resource is applied
// to every cluster the env deploys to, and drift is a per-cluster fact — the
// same Namespace can be correct on one cluster and missing from another. A
// shape that named it once could not say which.
func projectObjects(in ShapeInput) ([]release.ShapeObject, error) {
	stateful := map[string]bool{}
	for _, name := range in.StatefulWorkloads {
		stateful[name] = true
	}
	normalize := imageNormalizer(in.Images)

	var out []release.ShapeObject
	for i, doc := range splitStream(in.Manifests) {
		var body any
		if err := yaml.Unmarshal([]byte(doc.yaml), &body); err != nil {
			return nil, fmt.Errorf("shape: manifest document %d does not parse as YAML: %w", i+1, err)
		}
		if body == nil {
			continue
		}
		body = redactSecretValues(body)
		meta := readMeta(body)
		if meta.kind == "" || meta.name == "" {
			return nil, fmt.Errorf("shape: manifest document %d has no kind or metadata.name", i+1)
		}
		hash, err := hashDocument(body)
		if err != nil {
			return nil, fmt.Errorf("shape: hash %s %s: %w", meta.kind, meta.name, err)
		}
		configHash, err := hashDocument(normalize(body))
		if err != nil {
			return nil, fmt.Errorf("shape: config-hash %s %s: %w", meta.kind, meta.name, err)
		}
		obj := release.ShapeObject{
			APIVersion: meta.apiVersion,
			Kind:       meta.kind,
			Namespace:  meta.namespace,
			Name:       meta.name,
			Workload:   meta.workload,
			Hash:       hash,
			ConfigHash: configHash,
			Images:     imagesIn(doc.yaml, in.Images),
			Stateful:   stateful[meta.workload],
			Identity:   identityOf(meta.kind, body),
		}
		// A document attributed to no cluster is still part of the env's
		// shape — a host-only env renders objects nobody applies, and a
		// reader has to see them rather than have them silently dropped.
		if len(doc.clusters) == 0 {
			out = append(out, obj)
			continue
		}
		for _, c := range doc.clusters {
			perCluster := obj
			perCluster.Cluster = c
			out = append(out, perCluster)
		}
	}
	return out, nil
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

// ─── Hashing ────────────────────────────────────────────────────────────────

// hashDocument is the per-object hash: sha256 over the document's CANONICAL
// JSON.
//
// Canonical JSON rather than the rendered YAML, because the hash has to mean
// "this object" and not "these bytes". encoding/json sorts an object's keys,
// so a renderer that reorders a map, reflows a list or changes its
// indentation produces the same hash — while any change to a VALUE changes
// it. Comparing YAML text would make every cosmetic render change look like
// drift, which is the signal this hash exists to carry.
func hashDocument(body any) (string, error) {
	canonical, err := json.Marshal(jsonable(body))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// jsonable converts a YAML-decoded value into something encoding/json can
// marshal deterministically. yaml.v3 decodes a mapping into
// map[string]interface{} when the target is `any`, but a mapping with a
// non-string key decodes into map[interface{}]interface{}, which json
// refuses; such a key is stringified rather than failing the whole hash.
func jsonable(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = jsonable(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = jsonable(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = jsonable(val)
		}
		return out
	default:
		return v
	}
}

// ─── Secret redaction (F-13) ────────────────────────────────────────────────

// secretValueFields are the two places a Kubernetes Secret holds a value.
var secretValueFields = []string{"data", "stringData"}

// redactSecretValues replaces every value under a Secret's `data` /
// `stringData` with the hash of that value, in place of the value.
//
// It runs BEFORE the document is hashed, so no hash input ever contains a
// secret — which matters because a shape is stored forever, shown to every
// member of an org, and (once F2 lands) sealed inside a bundle that is
// cached and shared. A leak there is permanent.
//
// The KEYS survive. "which keys does this Secret carry" is a shape fact an
// operator needs; "what are they" is not, and the hash is what keeps
// "a secret value changed" visible without carrying the value.
func redactSecretValues(body any) any {
	doc, ok := body.(map[string]any)
	if !ok || doc["kind"] != "Secret" {
		return body
	}
	for _, field := range secretValueFields {
		values, ok := doc[field].(map[string]any)
		if !ok {
			continue
		}
		redacted := make(map[string]any, len(values))
		for key, value := range values {
			redacted[key] = redactValue(value)
		}
		doc[field] = redacted
	}
	return doc
}

func redactValue(value any) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(value)))
	return release.RedactedSecretPrefix + "sha256:" + hex.EncodeToString(sum[:])
}

// ─── Images and the config hash ─────────────────────────────────────────────

// artifactPlaceholder is what a release-bound image digest becomes in the
// document a ConfigHash is taken over.
const artifactPlaceholder = "forge.dev/artifact:"

// imagesIn reports which release artifacts' pinned digests appear in a
// document, as artifact key → digest.
//
// Searched as TEXT, over the whole document, because an image reference is
// not confined to a container's `image` field: forge's own operators carry
// the image they launch as an env-var value (control-plane's
// workspace-controller pins the workspace base image in DAEMON_IMAGE), and a
// chart's values can put one anywhere. A reader that only looked at pod specs
// would report such an object as carrying no image while a deploy very much
// changes which bytes it runs.
func imagesIn(doc string, images map[string]string) map[string]string {
	var found map[string]string
	for _, artifact := range sortedKeys(images) {
		digest := images[artifact]
		if digest == "" || !strings.Contains(doc, digest) {
			continue
		}
		if found == nil {
			found = map[string]string{}
		}
		found[artifact] = digest
	}
	return found
}

// imageNormalizer returns a function that rewrites every release-bound image
// digest in a document to its artifact key, which is what makes ConfigHash
// the identity of the deploy's SHAPE rather than of its images.
//
// Two bundles with equal ConfigHashes differ, at most, in which release they
// pin — so "promote a new release" and "the KCL moved" are told apart by one
// comparison instead of by re-rendering and diffing YAML.
//
// Only the digest is replaced, not the whole reference: the repository is
// part of the config (pushing an image somewhere else IS a change), and the
// digest is the only part a promotion moves. Artifacts are applied in sorted
// order so two artifacts that happen to share a digest normalize the same way
// every time.
func imageNormalizer(images map[string]string) func(any) any {
	replacements := make([]string, 0, 2*len(images))
	for _, artifact := range sortedKeys(images) {
		if digest := images[artifact]; digest != "" {
			replacements = append(replacements, digest, artifactPlaceholder+artifact)
		}
	}
	if len(replacements) == 0 {
		return func(body any) any { return body }
	}
	replacer := strings.NewReplacer(replacements...)
	return func(body any) any { return replaceStrings(body, replacer.Replace) }
}

// replaceStrings rewrites every string in a decoded document, returning a
// copy. A copy, because the caller still needs the original to hash.
func replaceStrings(v any, rewrite func(string) string) any {
	switch t := v.(type) {
	case string:
		return rewrite(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = replaceStrings(val, rewrite)
		}
		return out
	case map[any]any:
		out := make(map[any]any, len(t))
		for k, val := range t {
			out[k] = replaceStrings(val, rewrite)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = replaceStrings(val, rewrite)
		}
		return out
	default:
		return v
	}
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

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
