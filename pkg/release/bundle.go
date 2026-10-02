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
// blob is the [BundleDoc] and whose first layer is the manifest stream.
const (
	BundleArtifactType    = "application/vnd.forge.bundle.v1"
	BundleConfigMediaType = "application/vnd.forge.bundle.config.v1+json"
	BundleManifestsLayer  = "application/vnd.forge.bundle.manifests.v1.tar+gzip"
	BundleChartsLayer     = "application/vnd.forge.bundle.charts.v1.tar+gzip"
	// RedactedSecretPrefix marks a Secret value replaced by its hash before a
	// render is hashed or packaged: "forge.dev/redacted: sha256:<hex>".
	RedactedSecretPrefix = "forge.dev/redacted: "
)

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
	CreatedAt    time.Time  `json:"created_at"`
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
	// Digest is the OCI manifest digest: the bundle's identity.
	Digest string `json:"digest"`
	// Reference is repo@digest, or an OCI-layout reference for a file
	// ledger.
	Reference    string     `json:"reference"`
	ConfigDigest string     `json:"config_digest"`
	Shape        Shape      `json:"shape"`
	Provenance   Provenance `json:"provenance"`
	Run          Run        `json:"run,omitempty"`
	CreatedBy    string     `json:"created_by,omitempty"`
	ImportedFrom string     `json:"imported_from,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
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
