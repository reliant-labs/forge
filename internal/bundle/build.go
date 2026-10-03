package bundle

// Building a bundle: the immutable record of what was shipped, and the SOURCE
// THE RECONCILER APPLIES.
//
// One bundle is one OCI image manifest (doc §4.1):
//
//	manifest   artifactType application/vnd.forge.bundle.v1
//	 ├─ config  application/vnd.forge.bundle.config.v1+json   the BundleDoc
//	 └─ layer   ….bundle.manifests.v1.tar+gzip                manifests/<cluster>/<NNN>-<kind>-<name>.yaml
//
// IT IS FLUX'S SOURCE, WHICH IS WHY THE LAYOUT IS A CONTRACT. A Flux
// OCIRepository selects the one layer by media type with
// `operation: extract`, and a Kustomization per target cluster applies the
// YAML under [release.BundleClusterPath]'s answer for that cluster. forge
// writes those paths; the control plane reads them off the BundleDoc. Neither
// side re-derives them — see release.BundleClusterPath on why a second
// spelling fails silently and green.
//
// EXACTLY ONE LAYER, AND NO CHART LAYER. A layerSelector picks one layer, so a
// second one is invisible to the reconciler: anything in it would be recorded
// as shipped and never applied. The bundle therefore carries the env's own
// objects and nothing else.
//
// PLATFORM CHARTS ARE DELIBERATELY NOT IN HERE, and that is a safety property
// rather than a simplification. Every forge.HelmChart is by declaration a
// platform DEPENDENCY — Flux itself, the CNPG operator, Envoy Gateway with
// its Gateway API CRDs, cert-manager — and the hub Kustomization prunes what
// its path no longer carries. A bundle that owned those objects could delete
// its own reconciler, orphan every Postgres cluster whose CRD it removed, or
// revoke every certificate, and it would do so from a render that merely
// failed to select a chart. Platform infra stays on forge's explicit
// cluster-bootstrap path (`forge env deploy <env> --target <chart>`), where it
// is applied by a human naming it, against a cluster, once.
//
// EVERY BYTE IS A FUNCTION OF THE INPUT. The tar entries are sorted, their
// mtimes, uid/gid and modes are fixed, the gzip header carries no time, and
// the manifest's `created` annotation is pinned — so Build on the same render
// twice produces the same digest, and a re-push of an unchanged bundle is
// idempotent instead of cutting a second record of the same deploy.
// [BundleDoc.CreatedAt] is the one input that is a clock, and it is the
// CALLER's: Build never reads one, because a function that did could not be
// determinism-tested and its output would differ run to run by construction.
//
// The manifest layer is the stream a deploy APPLIES. Doc §4.4: the apply path
// reads the bundle's manifests rather than re-rendering, so "what was
// applied" and "what was recorded" are the same bytes. That is why the
// filenames carry the render's index — the apply order is a property of the
// stream (a Namespace before the objects in it) and a directory listing would
// lose it.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gopkg.in/yaml.v3"
	"oras.land/oras-go/v2/content"

	"github.com/reliant-labs/forge/pkg/release"
)

// Annotation keys on a bundle's OCI manifest (doc §4.1). The `org.*` ones are
// OCI standard, so a registry UI shows them; the `dev.forge.*` ones are what
// the retention policy and a human browsing a repository read.
const (
	AnnotationEnv          = "dev.forge.env"
	AnnotationProject      = "dev.forge.project"
	AnnotationRelease      = "dev.forge.release"
	AnnotationConfigDigest = "dev.forge.config-digest"
	AnnotationDirty        = "dev.forge.dirty"
	AnnotationWorktreeKey  = "dev.forge.worktree-key"
)

// manifestsPrefix is the directory the manifest layer's entries live under.
// pkg/release owns it, because the reconciler's path is built from it.
const manifestsPrefix = release.BundleManifestsPrefix

// fixedModTime pins every tar entry's timestamp. Unix zero rather than the
// build time: the bundle's digest must depend on the render and nothing else.
var fixedModTime = time.Unix(0, 0).UTC()

// fixedCreated pins the OCI manifest's `created` annotation, for the same
// reason. The bundle's real creation time is in the BundleDoc, where it is
// the caller's input rather than a clock read mid-build.
const fixedCreated = "1970-01-01T00:00:00Z"

const (
	tarFileMode = 0o644
	tarDirMode  = 0o755
)

// BuildInput is one env's render plus the facts that identify it.
type BuildInput struct {
	// Project and Env name the bundle. Both required: a bundle is one
	// env's render, and a reader that could not tell which env it
	// described would have an artifact it could not place.
	Project string
	Env     string
	// Release is the version pinned, or "" for an unreleased bundle (dev,
	// or a Preview deploy) pinned to the build's own digests.
	Release string
	// Pins is the pin set the render resolved: images by artifact key,
	// and source pins for source-built components.
	Pins release.BundlePins
	// Provenance is where the render came from. Captured by the caller
	// (release.CaptureProvenance), never by this package: provenance is a
	// property of a checkout and this package never sees one.
	Provenance release.Provenance
	// Shape is the declaration half of the projection, exactly as
	// [ProjectShape] takes it. Build runs the projection ITSELF, from the
	// same parse it packages, rather than accepting a finished shape —
	// which is what makes "the shape describes these manifests" true by
	// construction instead of by a caller's discipline.
	Shape ShapeInput
	// CreatedAt is the bundle's creation time. Required, and the caller's:
	// see the determinism note at the top of this file.
	CreatedAt time.Time
}

// Bundle is a built bundle, in memory: its identity, its document, and the
// blobs that constitute it.
//
// Held in memory rather than written to a path because the two things a
// caller does with it — push it to a registry, write it into a local OCI
// layout — both want the blobs, and a bundle is small by design (doc §4.1
// measures control-plane prod at 246 KB).
type Bundle struct {
	// Digest is the OCI manifest digest: the bundle's identity.
	Digest string
	// Doc is the config blob, decoded. [Bundle.ConfigBlob] is the bytes.
	Doc release.BundleDoc
	// Manifest is the OCI image manifest, as raw JSON — the bytes Digest
	// is over, so a consumer verifies against exactly what was hashed
	// rather than against a re-marshalling of the struct.
	Manifest []byte
	// blobs are the config and layer blobs, by descriptor.
	blobs []blob
}

type blob struct {
	desc ocispec.Descriptor
	data []byte
}

// ConfigBlob is the BundleDoc's canonical bytes — what
// [release.DecodeBundleDoc] reads and what the config descriptor's digest is
// over.
func (b Bundle) ConfigBlob() []byte {
	for _, bl := range b.blobs {
		if bl.desc.MediaType == release.BundleConfigMediaType {
			return bl.data
		}
	}
	return nil
}

// Layer returns the blob of a given media type.
func (b Bundle) Layer(mediaType string) ([]byte, bool) {
	for _, bl := range b.blobs {
		if bl.desc.MediaType == mediaType {
			return bl.data, true
		}
	}
	return nil, false
}

// Build renders one env's bundle: the manifest layer, the shape, the config
// digest, the BundleDoc, and the OCI manifest that binds them.
//
// It takes a context for symmetry with [Push] and because a large render is
// cancellable work; it performs no IO.
func Build(ctx context.Context, in BuildInput) (Bundle, error) {
	if err := ctx.Err(); err != nil {
		return Bundle{}, err
	}
	switch {
	case strings.TrimSpace(in.Project) == "":
		return Bundle{}, fmt.Errorf("%w: bundle project is required", release.ErrInvalid)
	case strings.TrimSpace(in.Env) == "":
		return Bundle{}, fmt.Errorf("%w: bundle env is required", release.ErrInvalid)
	case in.CreatedAt.IsZero():
		return Bundle{}, fmt.Errorf("%w: bundle created_at is required; Build never reads a clock", release.ErrInvalid)
	}

	// ONE parse, feeding both halves — see parse.go.
	docs, err := parseStream(in.Shape.Manifests)
	if err != nil {
		return Bundle{}, fmt.Errorf("bundle %s/%s: %w", in.Project, in.Env, err)
	}
	if !in.Shape.Kind.Valid() {
		return Bundle{}, fmt.Errorf("%w: bundle %s/%s: environment kind %q", release.ErrInvalid, in.Project, in.Env, in.Shape.Kind)
	}
	objects, err := shapeObjects(docs, in.Shape.Images, in.Shape.StatefulWorkloads)
	if err != nil {
		return Bundle{}, fmt.Errorf("bundle %s/%s: %w", in.Project, in.Env, err)
	}
	shape := release.Shape{
		Kind:      in.Shape.Kind,
		Workloads: in.Shape.Workloads,
		Secrets:   in.Shape.Secrets,
		Domains:   in.Shape.Domains,
		Clusters:  in.Shape.Clusters,
		Objects:   objects,
	}.Canonical()
	if err := shape.Validate(); err != nil {
		return Bundle{}, fmt.Errorf("bundle %s/%s: %w", in.Project, in.Env, err)
	}

	manifestsLayer, trees, err := packManifests(docs)
	if err != nil {
		return Bundle{}, fmt.Errorf("bundle %s/%s: pack manifests: %w", in.Project, in.Env, err)
	}
	configDigest, err := release.ConfigDigest(shape)
	if err != nil {
		return Bundle{}, fmt.Errorf("bundle %s/%s: %w", in.Project, in.Env, err)
	}

	doc := release.BundleDoc{
		Schema:       release.BundleSchema,
		Project:      in.Project,
		Env:          in.Env,
		Release:      in.Release,
		ConfigDigest: configDigest,
		Pins:         in.Pins,
		Provenance:   in.Provenance,
		Shape:        shape,
		ClusterPaths: trees,
		CreatedAt:    in.CreatedAt.UTC(),
	}
	if err := doc.Validate(); err != nil {
		return Bundle{}, fmt.Errorf("bundle %s/%s: %w", in.Project, in.Env, err)
	}
	configBlob, err := json.Marshal(doc)
	if err != nil {
		return Bundle{}, fmt.Errorf("bundle %s/%s: encode document: %w", in.Project, in.Env, err)
	}

	// F-13, THE LAST GATE. parseStream already redacted every Secret, so
	// this cannot fail unless that path broke — which is exactly why it is
	// checked here, over the FINAL bytes, rather than trusted. A bundle is
	// kept forever and shared; a value in one is a permanent leak, so the
	// invariant is proved against what will actually be written and not
	// against an intermediate value.
	if err := refuseSecretValues(manifestsLayer, configBlob); err != nil {
		return Bundle{}, fmt.Errorf("bundle %s/%s: %w", in.Project, in.Env, err)
	}

	configDesc := content.NewDescriptorFromBytes(release.BundleConfigMediaType, configBlob)
	manifestsDesc := content.NewDescriptorFromBytes(release.BundleManifestsLayer, manifestsLayer)
	blobs := []blob{{configDesc, configBlob}, {manifestsDesc, manifestsLayer}}
	// ONE layer. A Flux layerSelector selects exactly one, so a second
	// would be recorded as shipped and never applied — see the package
	// note on why platform charts are not here.
	layers := []ocispec.Descriptor{manifestsDesc}

	ociManifest := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: release.BundleArtifactType,
		Config:       configDesc,
		Layers:       layers,
		Annotations:  annotationsFor(doc),
	}
	raw, err := json.Marshal(ociManifest)
	if err != nil {
		return Bundle{}, fmt.Errorf("bundle %s/%s: encode OCI manifest: %w", in.Project, in.Env, err)
	}
	manifestDesc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, raw)
	return Bundle{
		Digest:   manifestDesc.Digest.String(),
		Doc:      doc,
		Manifest: raw,
		blobs:    append(blobs, blob{manifestDesc, raw}),
	}, nil
}

// annotationsFor is the manifest's annotation set (doc §4.1). Empty values
// are omitted rather than written blank: an annotation that is present and
// empty reads as "recorded as nothing", and a registry UI would show it.
func annotationsFor(doc release.BundleDoc) map[string]string {
	out := map[string]string{
		ocispec.AnnotationCreated:  fixedCreated,
		AnnotationEnv:              doc.Env,
		AnnotationProject:          doc.Project,
		AnnotationConfigDigest:     doc.ConfigDigest,
		AnnotationDirty:            fmt.Sprint(doc.Provenance.Dirty),
		ocispec.AnnotationRevision: doc.Provenance.Commit,
		ocispec.AnnotationSource:   doc.Provenance.Repo,
	}
	if doc.Release != "" {
		out[AnnotationRelease] = doc.Release
	}
	if key := doc.Provenance.Worktree.Key; key != "" {
		out[AnnotationWorktreeKey] = key
	}
	for k, v := range out {
		if v == "" {
			delete(out, k)
		}
	}
	return out
}

// ─── The manifest layer ─────────────────────────────────────────────────────

// manifestEntryName is `<cluster path>/<NNN>-<kind>-<name>.yaml` (doc §4.1),
// where the cluster path is [release.BundleClusterPath]'s — the same string
// the reconciler sets as Kustomization.spec.path.
//
// The NNN is the document's position in the render, zero-padded, because the
// apply ORDER is part of the stream's meaning — a Namespace must land before
// the objects in it — and an unpacked directory listing is sorted
// lexicographically. Padding is what keeps entry 10 after entry 9.
//
// kustomize applies a path's resources in the order it reads them, and a
// generated kustomization.yaml lists them in that lexicographic order, so the
// padding carries the render's ordering through Flux as well as through a
// local unpack.
func manifestEntryName(doc parsedDoc, cluster string) string {
	return fmt.Sprintf("%s/%03d-%s-%s.yaml",
		release.BundleClusterPath(cluster), doc.index,
		pathSegment(strings.ToLower(doc.meta.kind)), pathSegment(doc.meta.name))
}

// pathSegment sanitizes one path component. pkg/release owns the rule, so
// forge and the control plane cannot disagree about a cluster's path; this is
// the local spelling for the kind and name halves of a filename.
func pathSegment(s string) string { return release.BundlePathSegment(s) }

// packManifests writes the redacted documents into the deterministic tar.gz
// that is layer 0.
//
// The documents are re-SERIALIZED from the parsed, redacted bodies rather
// than copied from the render's text. That is the F-13 requirement: the
// rendered text of a Secret contains its value, so a layer built by copying
// text would ship one however carefully the shape was redacted. Re-
// serializing costs a canonical YAML encoding and buys the guarantee that no
// unredacted byte can reach a bundle.
// It returns the layer AND the per-cluster trees it wrote, which is what the
// BundleDoc publishes. The trees come from the entries actually written rather
// than from the env's declared cluster list: a declared cluster with no
// documents routed to it must not get a Kustomization, because an empty path
// reconciles green over an env nothing applied.
func packManifests(docs []parsedDoc) ([]byte, []release.BundleClusterTree, error) {
	type entry struct {
		name    string
		cluster string
		data    []byte
	}
	var entries []entry
	for _, doc := range docs {
		encoded, err := encodeDocument(doc.body)
		if err != nil {
			return nil, nil, fmt.Errorf("encode %s %s: %w", doc.meta.kind, doc.meta.name, err)
		}
		clusters := doc.clusters
		if len(clusters) == 0 {
			clusters = []string{""}
		}
		for _, cluster := range clusters {
			entries = append(entries, entry{
				name: manifestEntryName(doc, cluster), cluster: cluster, data: encoded,
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	files := make([]tarEntry, 0, len(entries))
	counts := map[string]int{}
	var order []string
	for i, e := range entries {
		// Two documents that render to the same (cluster, index, kind,
		// name) would silently collapse into one entry, and the layer
		// would carry fewer objects than the shape claims. The shape's
		// own duplicate check would not catch it: these are per-cluster
		// pairs, and the names are sanitized, so two different cluster
		// contexts can sanitize to the same segment.
		if i > 0 && entries[i-1].name == e.name {
			return nil, nil, fmt.Errorf("two rendered documents both map to %s", e.name)
		}
		files = append(files, tarEntry{name: e.name, data: e.data})
		if _, seen := counts[e.cluster]; !seen {
			order = append(order, e.cluster)
		}
		counts[e.cluster]++
	}
	layer, err := writeTarGz(files)
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(order)
	trees := make([]release.BundleClusterTree, 0, len(order))
	for _, cluster := range order {
		trees = append(trees, release.BundleClusterTree{
			Cluster:   cluster,
			Path:      release.BundleClusterPath(cluster),
			Documents: counts[cluster],
		})
	}
	return layer, trees, nil
}

// encodeDocument serializes one document back to YAML, deterministically.
//
// yaml.v3 emits a map's keys in sorted order, so the bytes are a function of
// the document's content rather than of its decode order — the same property
// [release.HashDocument] relies on, for the same reason.
func encodeDocument(body any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(body); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type tarEntry struct {
	name string
	data []byte
}

// writeTarGz writes a deterministic tar.gz: sorted entries, fixed mtimes,
// zero uid/gid, fixed modes, and no gzip header name or time.
//
// Every one of those is a thing that would otherwise vary run to run and
// change the digest — which would make a bundle's identity a function of when
// and where it was built, so an unchanged render would cut a new record on
// every build and "has this already been deployed" would always answer no.
func writeTarGz(entries []tarEntry) ([]byte, error) {
	sorted := append([]tarEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })

	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(gz)

	// Parent directories are written explicitly, once each and in order,
	// so the archive is well-formed for an extractor that does not create
	// them implicitly.
	seen := map[string]bool{}
	for _, e := range sorted {
		for _, dir := range parentDirs(e.name) {
			if seen[dir] {
				continue
			}
			seen[dir] = true
			if err := tw.WriteHeader(&tar.Header{
				Name: dir + "/", Typeflag: tar.TypeDir, Mode: tarDirMode,
				ModTime: fixedModTime, Format: tar.FormatPAX,
			}); err != nil {
				return nil, err
			}
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name, Typeflag: tar.TypeReg, Mode: tarFileMode,
			Size: int64(len(e.data)), ModTime: fixedModTime, Format: tar.FormatPAX,
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(e.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// parentDirs lists an entry's ancestor directories, outermost first.
func parentDirs(name string) []string {
	parts := strings.Split(name, "/")
	if len(parts) < 2 {
		return nil
	}
	var out []string
	for i := 1; i < len(parts); i++ {
		out = append(out, strings.Join(parts[:i], "/"))
	}
	return out
}
