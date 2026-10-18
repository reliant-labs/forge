package release

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// BundleSchema is the BundleDoc schema this package reads and writes.
const BundleSchema = "forge.dev/bundle/v1"

// Bundle OCI media types. One bundle is one OCI image manifest whose config
// blob is the [BundleDoc] and whose single layer is the manifest stream.
//
// THE MANIFEST LAYER IS WHAT FLUX APPLIES. A Flux OCIRepository selects it by
// media type and `operation: extract`, which untars it into the source
// controller's artifact; a Kustomization then applies the YAML under
// `spec.path`. So the layer's media type and its internal layout are a
// CONTRACT with the reconciler, not an internal detail — see
// [BundleClusterPath].
//
// There is deliberately no charts layer. A second layer would be invisible to
// Flux (a layerSelector picks exactly one), so anything put there would be
// recorded as shipped and never applied. Platform charts are a
// cluster-bootstrap concern and stay off the bundle entirely; see
// internal/bundle's package doc.
const (
	BundleArtifactType    = "application/vnd.forge.bundle.v1"
	BundleConfigMediaType = "application/vnd.forge.bundle.config.v1+json"
	BundleManifestsLayer  = "application/vnd.forge.bundle.manifests.v1.tar+gzip"
	// RedactedSecretPrefix marks a Secret value replaced by its hash before a
	// render is hashed or packaged: "forge.dev/redacted: sha256:<hex>".
	RedactedSecretPrefix = "forge.dev/redacted: "
)

// BundleManifestsPrefix is the directory the manifest layer's entries live
// under, inside the extracted layer. Every entry is at
// `<prefix>/<cluster>/<file>.yaml`.
const BundleManifestsPrefix = "manifests"

// bundleUnclusteredSegment is the path segment for a document attributed to
// NO cluster — a host-only env's objects, which are part of the render and
// which nothing applies.
//
// It is a real segment rather than placing such documents directly under the
// prefix, so every entry is at the same depth and a consumer walking the tree
// needs one case instead of two. `_` cannot collide with a kubectl context,
// which [BundleClusterPath] sanitizes to the same alphabet but can never
// reduce to a bare underscore: a context has at least one character that
// survives sanitization, and an empty one is not a cluster.
const bundleUnclusteredSegment = "_"

// The observability files a bundle carries beside its manifests (console v2
// contract K4), in the SAME layer, under a top-level directory the reconciler
// never applies.
//
// WHY THE SAME LAYER AND NOT A SECOND ONE. A Flux layerSelector selects exactly
// one layer, so a second layer is invisible to the reconciler — which is right
// for data nothing applies, but it would also change what a bundle IS (one
// manifest, one config, one layer) for every reader that counts on that, and
// Fetch refuses a layer it does not know. A Kustomization's spec.path is always
// [BundleClusterPath], under [BundleManifestsPrefix], so a sibling directory in
// the extracted artifact is never reached by anything that applies.
//
//	observability/dashboards/<name>.json   one renderer-neutral dashboard each
//	observability/metrics-schema.json      the metrics schema (forge.metrics/v1)
//
// A dashboard file is named <name>_gen.json when forge generated it and
// <name>.json when the project owns it; a project's file wins over the
// generated one of the same name, so a name never appears twice.
const (
	BundleObservabilityPrefix = "observability"
	BundleDashboardsPath      = BundleObservabilityPrefix + "/dashboards"
	BundleMetricsSchemaPath   = BundleObservabilityPrefix + "/metrics-schema.json"
)

// BundleClusterPath is the path, within the extracted manifests layer, that
// holds the documents a deploy routes to one cluster. It is what the control
// plane sets as `Kustomization.spec.path` for that cluster.
//
// THIS FUNCTION IS THE CONTRACT, AND BOTH SIDES CALL IT. forge writes the tar
// entries under it and the control plane's Kustomization builder reads it. Two
// spellings of the same rule would be a silent, total failure: a Kustomization
// whose path does not exist in the artifact reconciles to "nothing to apply"
// and reports Ready — so the env would look converged while running the
// previous release forever. That is why the sanitizer lives here, beside the
// media type, rather than in whichever package happened to write the tar.
//
// An empty cluster name is the unclustered tree. A multi-cluster env has one
// path per cluster and ONE bundle: a document routed to two clusters appears
// under both paths, because each cluster's Kustomization applies its own path
// and prunes anything it owns that the path no longer carries.
func BundleClusterPath(cluster string) string {
	segment := BundlePathSegment(cluster)
	if strings.TrimSpace(cluster) == "" {
		segment = bundleUnclusteredSegment
	}
	return BundleManifestsPrefix + "/" + segment
}

// BundlePathSegment makes one name safe as a single path component: anything
// that is not alphanumeric, dash, underscore or dot becomes a dash.
//
// A Kubernetes name cannot contain a separator, but a CLUSTER name is a
// kubectl context and very much can (`gke_project_region_name` is the tame
// case; a context is free-form). An unsanitized segment would put entries at
// an unexpected depth, and the unpacker would be the only thing standing
// between a context name and a path the archive should never ask for.
//
// Exported because the control plane sanitizes the same cluster names when it
// builds a Kustomization path, and a second implementation that disagreed on
// one character would point it at a path the artifact does not hold.
func BundlePathSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	// A segment of dots would be "." or "..", which name a directory
	// rather than a file.
	if strings.Trim(out, ".") == "" {
		return "-"
	}
	return out
}

// MaxShapeBytes bounds a shape's canonical JSON. It is what a hosted ledger
// indexes per bundle, and what Live reads per env in one round trip, so it is
// a summary and never the render itself.
const MaxShapeBytes = 256 << 10

// BundleDoc is the config blob of a bundle: what one env's render, from one
// checkout, pinned to one release, IS — without its manifests. It lives inside
// the content-addressed bundle, so the bundle digest covers it, and a backend
// that records a bundle reads the shape and provenance out of these verified
// bytes rather than trusting a request field that describes them.
type BundleDoc struct {
	Schema  string `json:"schema"`
	Project string `json:"project"`
	Env     string `json:"env"`
	// Release is the version the bundle pins, or "" for an unreleased
	// bundle (dev, or a Preview deploy) pinned to a build's own digests.
	Release string `json:"release"`
	// ConfigDigest is the digest of the render with every release-bound
	// image reference normalized to its artifact key: the shape of the
	// deploy independent of which release it pins. Two bundles with equal
	// ConfigDigests differ, at most, in images.
	ConfigDigest string     `json:"config_digest"`
	Pins         BundlePins `json:"pins"`
	Provenance   Provenance `json:"provenance"`
	Shape        Shape      `json:"shape"`
	// Objects describes Shape.Objects for reading — a summary line and a
	// digest per section of each ([ObjectSummary]) — so two bundles can say
	// what changed between them without either's manifests. Beside the shape
	// rather than in it: the shape is bounded and read on every Live page,
	// these are read on a release page. Empty on a bundle built before it.
	Objects []ObjectSummary `json:"objects,omitempty"`
	// ClusterPaths is one entry per path the manifest layer actually
	// carries: the cluster, and the path under which its documents live.
	//
	// THE BUNDLE DESCRIBES ITS OWN LAYOUT so the reconciler's operator
	// builds one Kustomization per cluster without re-deriving anything.
	// It could compute the paths from Shape.Clusters and
	// [BundleClusterPath], and that is exactly the coupling this removes:
	// Shape.Clusters is the env's DECLARED cluster list, while these are
	// the paths the layer HOLDS, and the two differ whenever a declared
	// cluster ends up with no documents routed to it. A Kustomization
	// built for the declared-but-empty cluster points at a path that does
	// not exist, which reconciles green over an env that was never
	// applied.
	//
	// So it is written from the tar entries themselves, after they are
	// written, and a reader can trust it the way it trusts the digest.
	ClusterPaths []BundleClusterTree `json:"cluster_paths,omitempty"`
	// Trees is the v2 layout: one entry per cluster × delivery unit × phase,
	// for every cluster that is not [FlatCluster]. Each carries its
	// dependsOn, its per-attempt timeout and what its health is judged on,
	// so a reconciler builds one Kustomization per entry without deriving
	// anything. A cluster listed here has a [BundleClusterTree] too (its
	// Documents is the sum of its trees'); a flat cluster has no entry.
	// Sorted by Path.
	Trees []BundleTree `json:"trees,omitempty"`
	// Secrets names the Secret objects the env requires in each cluster.
	// NAMES ONLY: a Secret never rides the manifest layer, not even
	// redacted, so the reconciler neither writes nor prunes one. `forge env
	// deploy` syncs them to the cluster itself, before pointing the
	// reconciler at this bundle; a hub-reconciled env reads this list to
	// know what its clusters must already hold.
	Secrets   []BundleSecretRef `json:"secrets,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// BundleSecretRef is one Secret the env requires: where it lives and what it
// is called. It carries no value and no key names.
type BundleSecretRef struct {
	// Cluster is the kubectl context the Secret belongs in, or "" when the
	// render attributed it to no cluster.
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// BundleClusterTree is one cluster's documents inside the manifest layer.
type BundleClusterTree struct {
	// Cluster is the kubectl context the documents are routed to, or ""
	// for the unclustered tree (objects nothing applies).
	Cluster string `json:"cluster,omitempty"`
	// Path is [BundleClusterPath]'s answer for Cluster: what a
	// Kustomization sets as spec.path.
	Path string `json:"path"`
	// Documents is how many documents the path holds. Carried so a reader
	// can tell an empty tree from an absent one without fetching the
	// layer, and so a Kustomization is never built over zero documents.
	Documents int `json:"documents"`
}

// BundlePins is the pin set a bundle was rendered with: the same two halves
// a promotion freezes.
type BundlePins struct {
	Images  map[string]string `json:"images,omitempty"`
	Sources map[string]Source `json:"sources,omitempty"`
}

// Validate checks the document. It is the check a backend runs on bytes it
// was handed, so it is strict about everything it can be strict about.
func (d BundleDoc) Validate() error {
	switch {
	case d.Schema != BundleSchema:
		return fmt.Errorf("%w: bundle schema %q (expected %s)", ErrInvalid, d.Schema, BundleSchema)
	case strings.TrimSpace(d.Env) == "":
		return fmt.Errorf("%w: bundle env is required", ErrInvalid)
	case !ValidDigest(d.ConfigDigest):
		return fmt.Errorf("%w: bundle config digest %q is not a canonical sha256 digest", ErrInvalid, d.ConfigDigest)
	case d.CreatedAt.IsZero():
		return fmt.Errorf("%w: bundle created_at is required", ErrInvalid)
	}
	for image, dg := range d.Pins.Images {
		if !ValidDigest(dg) {
			return fmt.Errorf("%w: bundle pin %q: %q is not a canonical digest", ErrInvalid, image, dg)
		}
	}
	if err := d.Provenance.Validate(); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if err := d.Shape.Validate(); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if err := ValidateObjectSummaries(d.Shape, d.Objects); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if err := validateClusterPaths(d.ClusterPaths); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if err := validateTrees(d.ClusterPaths, d.Trees); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	for _, sec := range d.Secrets {
		if strings.TrimSpace(sec.Name) == "" {
			return fmt.Errorf("%w: bundle names a Secret with no name", ErrInvalid)
		}
	}
	return nil
}

// validateClusterPaths checks the layout the document claims.
//
// Each path must be the one [BundleClusterPath] computes for its cluster, and
// no cluster or path may appear twice. Both rules exist because the reconciler
// TRUSTS these strings: it sets them as Kustomization.spec.path, and a path
// that disagrees with the function the artifact was written by points at
// nothing — which reconciles as "no resources" and reports Ready, so a wrong
// path here is an env that looks converged and was never applied. Checking it
// against the function is what makes that unrepresentable rather than merely
// unlikely.
//
// A tree with no documents is refused for the same reason: a Kustomization
// over an empty path is the same green-over-nothing outcome, so an empty tree
// must not be written rather than being written and skipped by every reader.
func validateClusterPaths(trees []BundleClusterTree) error {
	clusters, paths := map[string]bool{}, map[string]bool{}
	for _, t := range trees {
		want := BundleClusterPath(t.Cluster)
		if t.Path != want {
			return fmt.Errorf("%w: cluster %q claims path %q, but the layout puts it at %q",
				ErrInvalid, t.Cluster, t.Path, want)
		}
		if t.Documents <= 0 {
			return fmt.Errorf("%w: cluster path %s holds %d documents; an empty path would reconcile green over an env nothing applied",
				ErrInvalid, t.Path, t.Documents)
		}
		if clusters[t.Cluster] {
			return fmt.Errorf("%w: bundle names cluster %q twice", ErrInvalid, t.Cluster)
		}
		if paths[t.Path] {
			// Two DIFFERENT cluster names that sanitize to one
			// segment. Their documents share a path, so each
			// cluster's Kustomization would apply the other's
			// objects as well as its own.
			return fmt.Errorf("%w: cluster %q shares path %s with another cluster; two contexts that differ only in characters the path sanitizes would apply each other's objects",
				ErrInvalid, t.Cluster, t.Path)
		}
		clusters[t.Cluster], paths[t.Path] = true, true
	}
	return nil
}

// DecodeBundleDoc strictly decodes a bundle config blob: unknown fields and a
// foreign schema are refused rather than ignored, because a backend records
// what it decoded and must not record a summary of a document it only half
// understood.
func DecodeBundleDoc(data []byte) (BundleDoc, error) {
	var d BundleDoc
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return BundleDoc{}, fmt.Errorf("%w: bundle document: %v", ErrInvalid, err)
	}
	if dec.More() {
		return BundleDoc{}, fmt.Errorf("%w: bundle document has trailing data", ErrInvalid)
	}
	if err := d.Validate(); err != nil {
		return BundleDoc{}, err
	}
	return d, nil
}

// BundleRecord is a bundle as a LEDGER holds it: the document's indexable
// half plus where the bytes are and who recorded them. Immutable.
type BundleRecord struct {
	ID  string `json:"id"`
	Env string `json:"env"`
	// Release is the version pinned, "" for an unreleased bundle.
	Release string `json:"release,omitempty"`
	// Digest is the OCI MANIFEST digest: the bundle's identity, and the
	// revision a reconciler reports.
	//
	// THIS IS THE VALUE THE CONTROL PLANE WRITES TO THE HUB OCIRepository'S
	// `spec.ref.digest`, and therefore the value a convergence read compares
	// `Kustomization.status.lastAppliedRevision` against. Measured
	// read-only on the dev hub, the chain is one unchanged string:
	//
	//	spec.ref.digest  ==  status.artifact.revision  ==  lastAppliedRevision
	//	sha256:<64 hex>      sha256:<64 hex>              sha256:<64 hex>
	//
	// and this field is produced in exactly that form ([ValidDigest]
	// enforces it). So the comparison is a plain string equality, which is
	// what control-plane's fluxobserve.judge performs — no normalization,
	// no tag prefix, nothing to strip on either side.
	//
	// It is the MANIFEST digest rather than the manifests LAYER's, and the
	// distinction is the one thing to get right when the hub moves from
	// per-deployment config artifacts to `bundle.v1/<env>`: a bundle's
	// identity, what a promotion names, and what Flux resolves `ref.digest`
	// to are all this one value, while the layer digest is an internal
	// detail of the artifact that nothing outside the bundle addresses.
	// Selecting a layer with `layerSelector` changes which bytes land in the
	// artifact, not which revision the source reports.
	//
	// That last sentence is reasoned from Flux's model (the revision is the
	// resolved manifest digest, fixed before layer extraction) and NOT
	// measured: no OCIRepository on the dev hub sets a layerSelector today,
	// and confirming it needs a cluster that does. It is worth one assertion
	// in the first integration test that points a Kustomization at a bundle
	// — if it were ever false, every env would read as permanently
	// progressing while being perfectly converged.
	Digest string `json:"digest"`
	// Reference is repo@digest, or an OCI-layout reference for a file
	// ledger.
	Reference    string `json:"reference"`
	ConfigDigest string `json:"config_digest"`
	Shape        Shape  `json:"shape"`
	// Objects is the document's [BundleDoc.Objects], kept with the record so
	// a release comparison reads it without fetching the bundle.
	Objects      []ObjectSummary `json:"objects,omitempty"`
	Provenance   Provenance      `json:"provenance"`
	Run          Run             `json:"run,omitempty"`
	CreatedBy    string          `json:"created_by,omitempty"`
	ImportedFrom string          `json:"imported_from,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// ─── Shape ───────────────────────────────────────────────────────────────────

// Shape is the indexable projection of one env's render: what runs, where,
// which secrets it needs, which domains it binds, and one hash per object. It
// is computed in the SAME pass that writes the manifests, so the two cannot
// disagree, and it is small enough to store per bundle and read per env.
//
// IT NEVER CARRIES A VALUE. Secrets are names and providers; objects are
// identities and hashes (a Secret's hash is taken after its values are
// redacted to their own hashes). [Shape.Validate] refuses anything that could
// hold a value, because a shape is stored forever and shown to every member.
type Shape struct {
	Kind      EnvKind         `json:"kind"`
	Workloads []ShapeWorkload `json:"workloads"`
	Secrets   []ShapeSecret   `json:"secrets"`
	Domains   []string        `json:"domains"`
	Clusters  []string        `json:"clusters"`
	Objects   []ShapeObject   `json:"objects"`
	// ControlPlaneSecrets are the Secrets the env's declared control plane
	// supplies and renews (forge.ControlPlaneSecret): where each lands and what
	// to mint into it. It is how the control plane learns what to deliver, so
	// it is recorded on every ensure. Omitted when empty, so an env that
	// declares none encodes, and hashes, exactly as it did before the field.
	ControlPlaneSecrets []ShapeControlPlaneSecret `json:"control_plane_secrets,omitempty"`
	// Alerts are the env's resolved alert rules: its own (forge.AlertRule) plus
	// the generated defaults it did not disable, each tagged with its source.
	// The control plane evaluates them; a rollback restores the rules of the
	// release it returns to. Each entry serializes to pkg/alerting's RuleSpec.
	// Omitted when empty, like ControlPlaneSecrets.
	Alerts []ShapeAlert `json:"alerts,omitempty"`
	// AlertRoutes say which receiver an alert goes to, by label, first match
	// wins unless a route continues. Receivers are NAMES: their secrets live
	// in the control plane. Order is significant and is preserved.
	AlertRoutes []ShapeAlertRoute `json:"alert_routes,omitempty"`
	// ObservabilityPolicy is the env's resolved observability opt-out
	// (forge.Observability): what the author switched off. It is how the
	// platform learns what NOT to collect, enforced at the source (the node
	// agent, the operator's injection, the browser ingest). Omitted when the
	// env declares no observability or opts out of nothing, so every shape
	// recorded before the field encodes and hashes exactly as it did.
	ObservabilityPolicy *ShapeObservabilityPolicy `json:"observability_policy,omitempty"`
	// DeliveryPolicy is what the env's releases must prove before they ship
	// (forge.DeliveryPolicy): one control per hardening control, each with a
	// mode and its parameters. It is how the control plane learns what to
	// enforce. Omitted when every value is the default, so every shape
	// recorded before the field encodes and hashes exactly as it did; a
	// reader treats absence as [DefaultDeliveryPolicy], never as "off".
	DeliveryPolicy *ShapeDeliveryPolicy `json:"delivery_policy,omitempty"`
	// Identity is the env's managed identity as declared
	// (forge.ManagedIdentity): the instance the control plane's identity
	// reconciler converges, and which workloads consume it. Names and
	// destinations only; every issued value is delivered, never recorded.
	// Omitted when the env declares none, so every shape recorded before the
	// field encodes and hashes exactly as it did.
	Identity *ShapeIdentity `json:"identity,omitempty"`
	// Applier is who applies this env when it DECLARES one: "forge" for a real
	// environment none of whose delivery units has moved onto the hub yet. It
	// is a transitional state (see [ApplierForge]). Empty means undeclared, and
	// omitted, so every shape recorded before the field encodes and hashes as
	// it did.
	Applier string `json:"applier,omitempty"`
	// Units is who applies each delivery unit of the env: the split between the
	// units the hub converges and the units forge still applies directly. It is
	// recorded only when the env declares one (delivery units, or
	// hub_converges), and omitted otherwise, so every shape recorded before the
	// field encodes and hashes exactly as it did. Sorted by name.
	//
	// It is a DECLARATION about who applies, so it is not part of the config
	// digest: moving a unit onto the hub changes nobody's configuration.
	Units []ShapeUnit `json:"units,omitempty"`
}

// ApplierForge is [Shape.Applier] for an env forge applies by declaration. It is
// a transitional state: an env whose every delivery unit is still applied by
// forge. The end state is every unit converged by the hub ([UnitApplierHub]).
const ApplierForge = "forge"

// Who applies a delivery unit ([ShapeUnit.Applier]).
const (
	// UnitApplierForge: `forge env deploy` applies the unit's objects directly.
	UnitApplierForge = "forge"
	// UnitApplierHub: the control plane's hub converges the unit's trees from
	// the recorded bundle, and forge applies none of its objects.
	UnitApplierHub = "hub"
)

// ShapeUnit is one delivery unit and who applies it.
type ShapeUnit struct {
	Name    string `json:"name"`
	Applier string `json:"applier"`
}

// ShapeWorkload is one declared workload and where it runs.
type ShapeWorkload struct {
	Name string `json:"name"`
	// Runtime is forge's runtime discriminator: host | compose | cluster |
	// hosted (and the frontend runtimes for frontends).
	Runtime string `json:"runtime"`
	// Cluster is the kube context for a cluster workload.
	Cluster string `json:"cluster,omitempty"`
	// Artifact is the release artifact key its image comes from, if any.
	Artifact string `json:"artifact,omitempty"`
	Replicas *int   `json:"replicas,omitempty"`
}

// ShapeSecret is one declared secret: a NAME, never a value.
type ShapeSecret struct {
	Name string `json:"name"`
	// Provider is the env's secret provider for it: file | hosted |
	// external | … (forge's provider discriminator).
	Provider string `json:"provider"`
	// DeclaredBy are the workloads that read it.
	DeclaredBy []string `json:"declared_by,omitempty"`
}

// Control-plane secret kinds: what the control plane mints into the Secret.
const (
	// ControlPlaneSecretTelemetryToken is the per-environment OTLP ingest
	// token.
	ControlPlaneSecretTelemetryToken = "telemetry_token"
	// ControlPlaneSecretIdentity is a managed identity's outputs for the
	// workloads that consume it: issuer, audience, scopes, and per
	// application the client id, redirect and login URIs and the broker
	// token. Derived from Bundle.identity; never declared by hand.
	ControlPlaneSecretIdentity = "identity"
	// ControlPlaneSecretIdentityMachineKey is a machine user's key.
	ControlPlaneSecretIdentityMachineKey = "identity_machine_key"
	// ControlPlaneSecretIdentityJWKS is the snapshot of the instance's public
	// signing keys a validator falls back to when the issuer is unreachable.
	ControlPlaneSecretIdentityJWKS = "identity_jwks"
)

// ControlPlaneSecretKinds is every kind a control-plane secret can have.
var ControlPlaneSecretKinds = []string{
	ControlPlaneSecretIdentity, ControlPlaneSecretIdentityJWKS,
	ControlPlaneSecretIdentityMachineKey, ControlPlaneSecretTelemetryToken,
}

// ShapeControlPlaneSecret is one Secret the control plane supplies: a
// DESTINATION and a KIND, never a value. Keys names what the control plane
// writes under the Secret, so a reader knows the contract without the kind's
// definition.
type ShapeControlPlaneSecret struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Cluster is the kube context the Secret lands in, the same key the
	// shape's workloads and objects use.
	Cluster string `json:"cluster"`
	// ConnectedCluster is the name `forge cluster connect` registered: the
	// credential the control plane delivers with.
	ConnectedCluster string `json:"connected_cluster"`
	// Kind says what the control plane mints into it
	// (ControlPlaneSecretTelemetryToken).
	Kind string   `json:"kind"`
	Keys []string `json:"keys,omitempty"`
	// Required says a workload reads the Secret without an optional marker,
	// so its pods wait for the first delivery instead of starting without it.
	// The control plane reports such an env as waiting on the delivery.
	// Omitted when false, so a shape recorded before the field is unchanged.
	Required bool `json:"required,omitempty"`
}

// Alert rule sources.
const (
	// AlertSourceUser is a rule the project declares (forge.AlertRule).
	AlertSourceUser = "user"
	// AlertSourceDefault is one of forge's generated defaults.
	AlertSourceDefault = "default"
)

// The receivers every control plane provides, and the routes a shape carries
// when the author declares none: in-app and Slack, never a pager.
const (
	AlertReceiverInApp = "reliant-inapp"
	AlertReceiverSlack = "slack"
)

// MinAlertInterval is the evaluation-interval floor a shape may record.
const MinAlertInterval = time.Minute

// ShapeAlert is one resolved alert rule: the Prometheus rule shape with the
// condition inside Expr, a single ClickHouse SELECT whose every row is a firing
// instance (non-`value` columns are labels, `value` the sample). The json tags
// are pkg/alerting's RuleSpec, so a consumer decodes this straight into it;
// KeepFiringFor and DefaultID are extra fields RuleSpec ignores.
type ShapeAlert struct {
	Alert string `json:"alert"`
	Expr  string `json:"expr"`
	// For and Interval are Go duration strings ("5m", "1h30m").
	For           string `json:"for,omitempty"`
	Interval      string `json:"interval,omitempty"`
	KeepFiringFor string `json:"keep_firing_for,omitempty"`
	// Labels always carry `severity`: critical | warning | info.
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	// Source is AlertSourceUser or AlertSourceDefault.
	Source string `json:"source"`
	// DefaultID names the generated default this rule came from.
	DefaultID string `json:"default_id,omitempty"`
}

// ShapeAlertRoute routes alerts whose labels contain every Match pair (an empty
// Match matches all) to Receiver, a name the control plane resolves.
type ShapeAlertRoute struct {
	Match    map[string]string `json:"match,omitempty"`
	Receiver string            `json:"receiver"`
	Continue bool              `json:"continue,omitempty"`
}

// Key identifies the Secret within a shape: (cluster, namespace, name).
func (s ShapeControlPlaneSecret) Key() string {
	return s.Cluster + "/" + s.Namespace + "/" + s.Name
}

// Observability opt-out categories: the closed vocabulary of
// [ShapeObservabilityPolicy.Disabled]. "metadata" is customer metadata, the
// per-workload container data the platform's node agent collects (pod
// CPU/memory, restarts, last exit reason, events, image and version).
const (
	ObsCategoryTraces   = "traces"
	ObsCategoryMetrics  = "metrics"
	ObsCategoryLogs     = "logs"
	ObsCategoryProfiles = "profiles"
	ObsCategoryMetadata = "metadata"
	ObsCategoryBrowser  = "browser"
)

// ObsCategories is every opt-out category, sorted.
var ObsCategories = []string{
	ObsCategoryBrowser, ObsCategoryLogs, ObsCategoryMetadata,
	ObsCategoryMetrics, ObsCategoryProfiles, ObsCategoryTraces,
}

// ShapeObservabilityPolicy is the observability an env opted OUT of. Disabled
// lists categories, sorted; Enabled = false means everything is off (and
// Disabled then lists every category). Absent policy, or an empty Disabled
// with Enabled true, means everything is on.
type ShapeObservabilityPolicy struct {
	Enabled  bool     `json:"enabled"`
	Disabled []string `json:"disabled,omitempty"`
	// OptIn lists the categories the env turned ON that default off (only
	// [ObsOptInReplay] today). Absent = none opted in.
	OptIn []string `json:"opt_in,omitempty"`
}

// ObsOptInReplay is session replay: the browser SDK's recorded DOM events,
// dropped by the platform unless the env opts in.
const ObsOptInReplay = "replay"

// ObsOptIns is every opt-in category, sorted.
var ObsOptIns = []string{ObsOptInReplay}

// OptedIn reports whether the env turned an opt-in category on. Everything
// disabled is never opted in.
func (p *ShapeObservabilityPolicy) OptedIn(category string) bool {
	if p == nil || !p.Enabled {
		return false
	}
	for _, c := range p.OptIn {
		if c == category {
			return true
		}
	}
	return false
}

// Off reports whether category is opted out under this policy.
func (p *ShapeObservabilityPolicy) Off(category string) bool {
	if p == nil {
		return false
	}
	if !p.Enabled {
		return true
	}
	for _, d := range p.Disabled {
		if d == category {
			return true
		}
	}
	return false
}

// ShapeObject is one rendered Kubernetes object.
type ShapeObject struct {
	Cluster    string `json:"cluster"`
	APIVersion string `json:"api_version,omitempty"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	// Workload is the forge workload the object belongs to
	// (forge.dev/workload), for grouping.
	Workload string `json:"workload,omitempty"`
	// Hash is the digest of the pinned document after secret redaction:
	// what drift detection compares the live object against.
	Hash string `json:"hash"`
	// ConfigHash is the digest of the same document with release-bound
	// image references normalized to their artifact keys. Equal ConfigHash
	// with a different Hash means only images changed.
	ConfigHash string `json:"config_hash,omitempty"`
	// Images maps the release artifact key → pinned digest for every
	// release-bound image the object runs.
	Images map[string]string `json:"images,omitempty"`
	// Stateful marks an object that holds data beyond its own spec — a
	// declared database's objects. The well-known stateful kinds
	// ([statefulKinds]) are recognised without it.
	Stateful bool `json:"stateful,omitempty"`
	// Tree is the object's place in a v2 bundle, "<unit>/<phase>", or ""
	// for an object on a flat cluster. An object whose Tree changes between
	// releases is MOVING between Kustomizations: the one it leaves prunes
	// it, the one it joins creates it. The plan stops on that
	// ([FindingTreeMoved]).
	Tree string `json:"tree,omitempty"`
	// Identity carries the attributes that give an object an EXTERNAL
	// identity, which a deploy can silently replace: a Service's type and
	// load balancer, an Ingress's static IP. Keys are closed
	// ([IdentityKeys]) so the map can never become a place a value hides.
	Identity map[string]string `json:"identity,omitempty"`
}

// Identity keys. Closed.
const (
	IdentityType              = "type"
	IdentityLoadBalancerIP    = "load_balancer_ip"
	IdentityLoadBalancerClass = "load_balancer_class"
	IdentityStaticIP          = "static_ip"
)

// IdentityKeys is the closed set of [ShapeObject.Identity] keys.
var IdentityKeys = []string{IdentityType, IdentityLoadBalancerIP, IdentityLoadBalancerClass, IdentityStaticIP}

// ObjectKey is an object's identity within a shape: (cluster, kind,
// namespace, name). The API version is deliberately not part of it — moving a
// Deployment from apps/v1beta2 to apps/v1 is a change to the same object.
type ObjectKey struct {
	Cluster   string `json:"cluster"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// String renders the key as "<cluster>/<kind>/<namespace>/<name>".
func (k ObjectKey) String() string {
	return k.Cluster + "/" + k.Kind + "/" + k.Namespace + "/" + k.Name
}

// Key is the object's [ObjectKey].
func (o ShapeObject) Key() ObjectKey {
	return ObjectKey{Cluster: o.Cluster, Kind: o.Kind, Namespace: o.Namespace, Name: o.Name}
}

// Validate checks a shape: a known kind, well-formed object identities, no
// duplicate objects, canonical hashes, and nothing that could carry a value.
func (s Shape) Validate() error {
	if !s.Kind.Valid() {
		return fmt.Errorf("%w: shape kind %q (expected one of %s)", ErrInvalid, s.Kind, strings.Join(envKindNames(), ", "))
	}
	seen := map[ObjectKey]bool{}
	for i, o := range s.Objects {
		if o.Kind == "" || o.Name == "" {
			return fmt.Errorf("%w: shape object %d has no kind or name", ErrInvalid, i)
		}
		if !ValidDigest(o.Hash) {
			return fmt.Errorf("%w: shape object %s: hash %q is not a canonical digest", ErrInvalid, o.Key(), o.Hash)
		}
		if o.ConfigHash != "" && !ValidDigest(o.ConfigHash) {
			return fmt.Errorf("%w: shape object %s: config hash %q is not a canonical digest", ErrInvalid, o.Key(), o.ConfigHash)
		}
		for artifact, d := range o.Images {
			if !ValidDigest(d) {
				return fmt.Errorf("%w: shape object %s: image %q pins %q, not a canonical digest", ErrInvalid, o.Key(), artifact, d)
			}
		}
		for k := range o.Identity {
			if !containsString(IdentityKeys, k) {
				return fmt.Errorf("%w: shape object %s: identity key %q is not one of %s", ErrInvalid, o.Key(), k, strings.Join(IdentityKeys, ", "))
			}
		}
		if seen[o.Key()] {
			return fmt.Errorf("%w: shape names object %s twice", ErrInvalid, o.Key())
		}
		seen[o.Key()] = true
	}
	names := map[string]bool{}
	for _, sec := range s.Secrets {
		if strings.TrimSpace(sec.Name) == "" {
			return fmt.Errorf("%w: shape secret with no name", ErrInvalid)
		}
		if names[sec.Name] {
			return fmt.Errorf("%w: shape names secret %q twice", ErrInvalid, sec.Name)
		}
		names[sec.Name] = true
	}
	for _, w := range s.Workloads {
		if strings.TrimSpace(w.Name) == "" || strings.TrimSpace(w.Runtime) == "" {
			return fmt.Errorf("%w: shape workload needs a name and a runtime", ErrInvalid)
		}
	}
	if s.Applier != "" && s.Applier != ApplierForge {
		return fmt.Errorf("%w: shape applier %q (expected %q or none)", ErrInvalid, s.Applier, ApplierForge)
	}
	seenUnit := map[string]bool{}
	for _, u := range s.Units {
		switch {
		case strings.TrimSpace(u.Name) == "":
			return fmt.Errorf("%w: shape unit with no name", ErrInvalid)
		case u.Applier != UnitApplierForge && u.Applier != UnitApplierHub:
			return fmt.Errorf("%w: shape unit %q applier %q (expected %q or %q)", ErrInvalid, u.Name, u.Applier, UnitApplierForge, UnitApplierHub)
		case seenUnit[u.Name]:
			return fmt.Errorf("%w: shape names unit %q twice", ErrInvalid, u.Name)
		}
		seenUnit[u.Name] = true
	}
	if p := s.ObservabilityPolicy; p != nil {
		valid := map[string]bool{}
		for _, c := range ObsCategories {
			valid[c] = true
		}
		seen := map[string]bool{}
		for _, d := range p.Disabled {
			if !valid[d] {
				return fmt.Errorf("%w: shape observability_policy disables %q (expected one of %s)", ErrInvalid, d, strings.Join(ObsCategories, ", "))
			}
			if seen[d] {
				return fmt.Errorf("%w: shape observability_policy disables %q twice", ErrInvalid, d)
			}
			seen[d] = true
		}
		optIns := map[string]bool{}
		for _, c := range ObsOptIns {
			optIns[c] = true
		}
		seenIn := map[string]bool{}
		for _, c := range p.OptIn {
			if !optIns[c] {
				return fmt.Errorf("%w: shape observability_policy opts in to %q (expected one of %s)", ErrInvalid, c, strings.Join(ObsOptIns, ", "))
			}
			if seenIn[c] {
				return fmt.Errorf("%w: shape observability_policy opts in to %q twice", ErrInvalid, c)
			}
			seenIn[c] = true
		}
		if !p.Enabled && len(p.Disabled) != len(ObsCategories) {
			return fmt.Errorf("%w: shape observability_policy is disabled as a whole and must list every category", ErrInvalid)
		}
	}
	if s.DeliveryPolicy != nil {
		if err := s.DeliveryPolicy.canonical().validate(); err != nil {
			return err
		}
	}
	delivered := map[string]bool{}
	for _, c := range s.ControlPlaneSecrets {
		if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Namespace) == "" || strings.TrimSpace(c.Cluster) == "" {
			return fmt.Errorf("%w: shape control-plane secret needs a name, a namespace and a cluster", ErrInvalid)
		}
		if strings.TrimSpace(c.ConnectedCluster) == "" {
			return fmt.Errorf("%w: shape control-plane secret %s names no connected cluster, so the control plane has no credential to deliver it with", ErrInvalid, c.Key())
		}
		if !containsString(ControlPlaneSecretKinds, c.Kind) {
			return fmt.Errorf("%w: shape control-plane secret %s: kind %q (expected one of %s)", ErrInvalid, c.Key(), c.Kind, strings.Join(ControlPlaneSecretKinds, ", "))
		}
		if c.Kind == ControlPlaneSecretIdentity && len(c.Keys) == 0 {
			return fmt.Errorf("%w: shape control-plane secret %s: an %s secret names its keys, because they depend on the applications", ErrInvalid, c.Key(), ControlPlaneSecretIdentity)
		}
		if delivered[c.Key()] {
			return fmt.Errorf("%w: shape names control-plane secret %s twice", ErrInvalid, c.Key())
		}
		delivered[c.Key()] = true
	}
	if s.Identity != nil {
		if err := s.Identity.validate(s.ControlPlaneSecrets); err != nil {
			return err
		}
	} else {
		for _, c := range s.ControlPlaneSecrets {
			if IsIdentityKind(c.Kind) {
				return fmt.Errorf("%w: shape control-plane secret %s has the identity kind %q, and the shape declares no identity", ErrInvalid, c.Key(), c.Kind)
			}
		}
	}
	return s.validateAlerts()
}

// validateAlerts is the shape-side check of the declared rules. KCL already
// refuses these at render; the shape re-checks because it is stored forever
// and read by a different program than the one that wrote it.
func (s Shape) validateAlerts() error {
	names := map[string]bool{}
	for _, a := range s.Alerts {
		switch {
		case strings.TrimSpace(a.Alert) == "":
			return fmt.Errorf("%w: shape alert with no name", ErrInvalid)
		case names[a.Alert]:
			return fmt.Errorf("%w: shape names alert %q twice", ErrInvalid, a.Alert)
		case strings.TrimSpace(a.Expr) == "":
			return fmt.Errorf("%w: shape alert %q has no expr", ErrInvalid, a.Alert)
		case strings.Contains(a.Expr, ";"):
			return fmt.Errorf("%w: shape alert %q: expr must be a single statement", ErrInvalid, a.Alert)
		case a.Source != AlertSourceUser && a.Source != AlertSourceDefault:
			return fmt.Errorf("%w: shape alert %q: source %q (expected %s or %s)", ErrInvalid, a.Alert, a.Source, AlertSourceUser, AlertSourceDefault)
		}
		names[a.Alert] = true
		for _, d := range []struct{ field, v string }{{"for", a.For}, {"interval", a.Interval}, {"keep_firing_for", a.KeepFiringFor}} {
			if d.v == "" {
				continue
			}
			dur, err := time.ParseDuration(d.v)
			if err != nil || dur < 0 {
				return fmt.Errorf("%w: shape alert %q: %s %q is not a duration", ErrInvalid, a.Alert, d.field, d.v)
			}
			if d.field == "interval" && dur < MinAlertInterval {
				return fmt.Errorf("%w: shape alert %q: interval %q is below the %s floor", ErrInvalid, a.Alert, d.v, MinAlertInterval)
			}
		}
		switch a.Labels["severity"] {
		case "critical", "warning", "info":
		default:
			return fmt.Errorf("%w: shape alert %q: severity label %q (expected critical, warning or info)", ErrInvalid, a.Alert, a.Labels["severity"])
		}
		for k := range a.Labels {
			if k == "alertname" || strings.HasPrefix(k, "reliant_") {
				return fmt.Errorf("%w: shape alert %q: label %q is reserved", ErrInvalid, a.Alert, k)
			}
		}
	}
	if len(s.Alerts) == 0 && len(s.AlertRoutes) > 0 {
		return fmt.Errorf("%w: shape has alert routes and no alerts", ErrInvalid)
	}
	hasDefault := false
	for i, r := range s.AlertRoutes {
		if strings.TrimSpace(r.Receiver) == "" {
			return fmt.Errorf("%w: shape alert route %d has no receiver", ErrInvalid, i)
		}
		if strings.Contains(r.Receiver, "://") {
			return fmt.Errorf("%w: shape alert route %d: receiver %q is a URL; a route names a receiver", ErrInvalid, i, r.Receiver)
		}
		if len(r.Match) == 0 {
			hasDefault = true
		}
	}
	if len(s.AlertRoutes) > 0 && !hasDefault {
		return fmt.Errorf("%w: shape alert routes have no default (empty-match) route", ErrInvalid)
	}
	return nil
}

// Canonical returns the shape with every list sorted, so two projections of
// the same render encode to the same bytes regardless of render order.
func (s Shape) Canonical() Shape {
	out := s
	out.Workloads = append([]ShapeWorkload(nil), s.Workloads...)
	sort.Slice(out.Workloads, func(i, j int) bool { return out.Workloads[i].Name < out.Workloads[j].Name })
	out.Secrets = make([]ShapeSecret, len(s.Secrets))
	for i, sec := range s.Secrets {
		sec.DeclaredBy = sortedStrings(sec.DeclaredBy)
		out.Secrets[i] = sec
	}
	sort.Slice(out.Secrets, func(i, j int) bool { return out.Secrets[i].Name < out.Secrets[j].Name })
	if len(s.ControlPlaneSecrets) > 0 {
		out.ControlPlaneSecrets = make([]ShapeControlPlaneSecret, len(s.ControlPlaneSecrets))
		for i, c := range s.ControlPlaneSecrets {
			c.Keys = sortedStrings(c.Keys)
			out.ControlPlaneSecrets[i] = c
		}
		sort.Slice(out.ControlPlaneSecrets, func(i, j int) bool {
			return out.ControlPlaneSecrets[i].Key() < out.ControlPlaneSecrets[j].Key()
		})
	}
	if s.ObservabilityPolicy != nil {
		pol := *s.ObservabilityPolicy
		pol.Disabled = sortedStrings(pol.Disabled)
		pol.OptIn = sortedStrings(pol.OptIn)
		out.ObservabilityPolicy = &pol
	}
	// The default policy is the absence of one: a producer that spells every
	// default out must hash the same as one that declares nothing.
	if s.DeliveryPolicy != nil {
		if pol := s.DeliveryPolicy.canonical(); !s.DeliveryPolicy.IsDefault() {
			out.DeliveryPolicy = &pol
		} else {
			out.DeliveryPolicy = nil
		}
	}
	if s.Identity != nil {
		id := s.Identity.canonical()
		out.Identity = &id
	}
	// Alerts sort by name. Routes do NOT sort: first match wins, so their order
	// is part of their meaning.
	if len(s.Alerts) > 0 {
		out.Alerts = append([]ShapeAlert(nil), s.Alerts...)
		sort.Slice(out.Alerts, func(i, j int) bool { return out.Alerts[i].Alert < out.Alerts[j].Alert })
	}
	if len(s.Units) > 0 {
		out.Units = append([]ShapeUnit(nil), s.Units...)
		sort.Slice(out.Units, func(i, j int) bool { return out.Units[i].Name < out.Units[j].Name })
	}
	out.Domains = sortedStrings(s.Domains)
	out.Clusters = sortedStrings(s.Clusters)
	out.Objects = append([]ShapeObject(nil), s.Objects...)
	sort.Slice(out.Objects, func(i, j int) bool { return out.Objects[i].Key().String() < out.Objects[j].Key().String() })
	// Encode empty lists as [] rather than null: a reader must never have
	// to tell "no secrets" from "not recorded" by null-ness.
	if out.Workloads == nil {
		out.Workloads = []ShapeWorkload{}
	}
	if out.Domains == nil {
		out.Domains = []string{}
	}
	if out.Clusters == nil {
		out.Clusters = []string{}
	}
	if out.Objects == nil {
		out.Objects = []ShapeObject{}
	}
	return out
}

// Encode is the canonical JSON of the shape, refused when it exceeds
// [MaxShapeBytes].
func (s Shape) Encode() ([]byte, error) {
	b, err := json.Marshal(s.Canonical())
	if err != nil {
		return nil, err
	}
	if len(b) > MaxShapeBytes {
		return nil, fmt.Errorf("%w: shape is %d bytes, over the %d-byte bound", ErrInvalid, len(b), MaxShapeBytes)
	}
	return b, nil
}

// SecretNamed returns the shape's secret of that name.
func (s Shape) SecretNamed(name string) (ShapeSecret, bool) {
	for _, sec := range s.Secrets {
		if sec.Name == name {
			return sec, true
		}
	}
	return ShapeSecret{}, false
}

func sortedStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func containsString(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

// BundleHostedCluster is the cluster key under which a bundle carries an env's
// HOSTED tier records (forge.dev Workload / ManagedDatabase / StaticSite).
//
// Hosted workloads run on the platform's own clusters, which forge cannot name
// as a kube context, so they live under ONE well-known tree,
// BundleClusterPath(BundleHostedCluster). The control plane maps that tree to
// the placement it resolved for the environment; forge never says where.
const BundleHostedCluster = "hosted"
