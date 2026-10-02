package bundle

// Building a bundle: the immutable record of what was shipped.
//
// One bundle is one OCI image manifest (doc §4.1):
//
//	manifest   artifactType application/vnd.forge.bundle.v1
//	 ├─ config  application/vnd.forge.bundle.config.v1+json   the BundleDoc
//	 ├─ layer 0 ….bundle.manifests.v1.tar+gzip                manifests/<cluster>/<NNN>-<kind>-<name>.yaml
//	 └─ layer 1 ….bundle.charts.v1.tar+gzip                   optional, helm template output
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
// Named rather than bare so an unpacked bundle can grow a second tree (the
// charts layer uses `charts/`) without the two colliding.
const manifestsPrefix = "manifests"

// chartsPrefix is the charts layer's directory.
const chartsPrefix = "charts"

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
	// Charts is the optional `helm template` output for declared
	// forge.HelmChart entities, keyed by chart name. Absent with
	// --no-charts.
	Charts map[string]string
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

	manifestsLayer, err := packManifests(docs)
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
	layers := []ocispec.Descriptor{manifestsDesc}

	if len(in.Charts) > 0 {
		chartsLayer, cerr := packCharts(in.Charts)
		if cerr != nil {
			return Bundle{}, fmt.Errorf("bundle %s/%s: pack charts: %w", in.Project, in.Env, cerr)
		}
		if cerr := refuseSecretValues(chartsLayer); cerr != nil {
			return Bundle{}, fmt.Errorf("bundle %s/%s: charts: %w", in.Project, in.Env, cerr)
		}
		chartsDesc := content.NewDescriptorFromBytes(release.BundleChartsLayer, chartsLayer)
		blobs = append(blobs, blob{chartsDesc, chartsLayer})
		layers = append(layers, chartsDesc)
	}

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

// manifestEntryName is `manifests/<cluster>/<NNN>-<kind>-<name>.yaml`
// (doc §4.1).
//
// The NNN is the document's position in the render, zero-padded, because the
// apply ORDER is part of the stream's meaning — a Namespace must land before
// the objects in it — and an unpacked directory listing is sorted
// lexicographically. Padding is what keeps entry 10 after entry 9.
//
// A document attributed to no cluster goes under `_` rather than directly
// under `manifests/`, so every entry is at the same depth and a consumer
// walking the tree does not need two cases.
func manifestEntryName(doc parsedDoc, cluster string) string {
	if cluster == "" {
		cluster = "_"
	}
	return fmt.Sprintf("%s/%s/%03d-%s-%s.yaml",
		manifestsPrefix, pathSegment(cluster), doc.index,
		pathSegment(strings.ToLower(doc.meta.kind)), pathSegment(doc.meta.name))
}

// pathSegment makes one name safe as a single path component: anything that
// is not alphanumeric, dash, underscore or dot becomes a dash.
//
// A Kubernetes name cannot contain a separator, but a CLUSTER name is a
// kubectl context and very much can (`gke_project_region_name` is the tame
// case; a context is free-form). An unsanitized segment would put entries at
// an unexpected depth, and the unpacker would be the only thing standing
// between a context name and a path the archive should never ask for.
func pathSegment(s string) string {
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

// packManifests writes the redacted documents into the deterministic tar.gz
// that is layer 0.
//
// The documents are re-SERIALIZED from the parsed, redacted bodies rather
// than copied from the render's text. That is the F-13 requirement: the
// rendered text of a Secret contains its value, so a layer built by copying
// text would ship one however carefully the shape was redacted. Re-
// serializing costs a canonical YAML encoding and buys the guarantee that no
// unredacted byte can reach a bundle.
func packManifests(docs []parsedDoc) ([]byte, error) {
	type entry struct {
		name string
		data []byte
	}
	var entries []entry
	for _, doc := range docs {
		encoded, err := encodeDocument(doc.body)
		if err != nil {
			return nil, fmt.Errorf("encode %s %s: %w", doc.meta.kind, doc.meta.name, err)
		}
		clusters := doc.clusters
		if len(clusters) == 0 {
			clusters = []string{""}
		}
		for _, cluster := range clusters {
			entries = append(entries, entry{name: manifestEntryName(doc, cluster), data: encoded})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	files := make([]tarEntry, 0, len(entries))
	for i, e := range entries {
		// Two documents that render to the same (cluster, index, kind,
		// name) would silently collapse into one entry, and the layer
		// would carry fewer objects than the shape claims. The shape's
		// own duplicate check would not catch it: these are per-cluster
		// pairs, and the names are sanitized, so two different cluster
		// contexts can sanitize to the same segment.
		if i > 0 && entries[i-1].name == e.name {
			return nil, fmt.Errorf("two rendered documents both map to %s", e.name)
		}
		files = append(files, tarEntry{name: e.name, data: e.data})
	}
	return writeTarGz(files)
}

// packCharts writes `helm template` output into layer 1, one file per chart.
func packCharts(charts map[string]string) ([]byte, error) {
	names := make([]string, 0, len(charts))
	for name := range charts {
		names = append(names, name)
	}
	sort.Strings(names)
	files := make([]tarEntry, 0, len(names))
	for _, name := range names {
		files = append(files, tarEntry{
			name: fmt.Sprintf("%s/%s.yaml", chartsPrefix, pathSegment(name)),
			data: []byte(charts[name]),
		})
	}
	return writeTarGz(files)
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
