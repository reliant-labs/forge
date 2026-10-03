package bundle

// ONE pass over the render, feeding both halves of a bundle.
//
// The shape and the manifest layer are produced from the same parsed
// documents, in the same order, with the same redaction already applied. That
// is not an optimization — it is the invariant doc §4.2 names: "`shape` is
// computed by the same function that writes the manifests, so the two cannot
// disagree". A bundle whose shape described objects its layer did not carry
// would be a record of a deploy that never happened, and nothing downstream
// could detect it: both halves are sealed under the same digest, so they
// would be consistently wrong.
//
// It is also what makes F-13 structural. [parseStream] redacts every Secret
// value on the way through, so a caller cannot obtain an UNREDACTED document
// from this package at all. [Build]'s canary check is then a proof that the
// one path works, not a filter that has to catch what a second path let
// through.

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/pkg/release"
)

// parsedDoc is one rendered document, decoded, redacted, and placed.
type parsedDoc struct {
	// index is the document's 0-based position in the rendered stream,
	// which IS its apply order: a Namespace before the objects in it, a
	// CRD before its custom resources. The manifest layer's filenames
	// carry it so an unpacked bundle applies in the order it was rendered
	// rather than in whatever order a directory listing returns.
	index int
	// body is the decoded document AFTER secret redaction.
	body any
	meta docMeta
	// clusters are the clusters the deploy layer routes this document to,
	// read off the stream's `# cluster:` header. Empty means the document
	// is attributed to no cluster.
	clusters []string
	// rendered is the document's original YAML text, which
	// [release.ImagesIn] searches. It is the UNREDACTED text and is never
	// written anywhere: a Secret's text would carry its value.
	rendered string
}

// parseStream decodes the annotated manifest stream into documents, redacting
// every Secret value as it goes.
func parseStream(stream string) ([]parsedDoc, error) {
	var out []parsedDoc
	for i, doc := range splitStream(stream) {
		var body any
		if err := yaml.Unmarshal([]byte(doc.yaml), &body); err != nil {
			return nil, fmt.Errorf("manifest document %d does not parse as YAML: %w", i+1, err)
		}
		if body == nil {
			continue
		}
		body = release.RedactSecrets(body)
		meta := readMeta(body)
		if meta.kind == "" || meta.name == "" {
			return nil, fmt.Errorf("manifest document %d has no kind or metadata.name", i+1)
		}
		out = append(out, parsedDoc{
			index:    len(out),
			body:     body,
			meta:     meta,
			clusters: doc.clusters,
			rendered: doc.yaml,
		})
	}
	return out, nil
}

// shapeObjects projects parsed documents into one [release.ShapeObject] per
// (document, cluster) pair.
//
// PER PAIR, not per document: an unattributed env-level resource is applied
// to every cluster the env deploys to, and drift is a per-cluster fact — the
// same Namespace can be correct on one cluster and missing from another. A
// shape that named it once could not say which.
func shapeObjects(docs []parsedDoc, images map[string]string, statefulWorkloads []string) ([]release.ShapeObject, error) {
	stateful := map[string]bool{}
	for _, name := range statefulWorkloads {
		stateful[name] = true
	}
	var out []release.ShapeObject
	for _, doc := range docs {
		hash, configHash, err := release.ObjectHashes(doc.body, images)
		if err != nil {
			return nil, fmt.Errorf("hash %s %s: %w", doc.meta.kind, doc.meta.name, err)
		}
		obj := release.ShapeObject{
			APIVersion: doc.meta.apiVersion,
			Kind:       doc.meta.kind,
			Namespace:  doc.meta.namespace,
			Name:       doc.meta.name,
			Workload:   doc.meta.workload,
			Hash:       hash,
			ConfigHash: configHash,
			Images:     release.ImagesIn(doc.rendered, images),
			Stateful:   stateful[doc.meta.workload],
			Identity:   identityOf(doc.meta.kind, doc.body),
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
	return collapseIdentical(out)
}

// collapseIdentical resolves two documents that land on ONE ObjectKey, which
// is what a deploy does to them and therefore what the projection must say.
//
// This agrees with `kubectl apply`, which is the projection's whole contract.
// Two IDENTICAL documents under one key are indistinguishable to a deploy:
// applying the same bytes twice leaves the same object, so the shape names it
// once. That case is legitimate — two independent declarations can each
// require a namespace to exist, and forge's own render replicates an
// unattributed env-level object to every cluster.
//
// Two DIFFERENT bodies under one key are REFUSED, because `kubectl apply` is
// last-wins: the surviving object depends on document order, so the render
// silently decides which writer won. That is the "two writers, one object"
// conflict a shape exists to expose, and a shape that merged it would record
// a deploy nobody declared.
//
// The error names the key AND what differs, because the question a reader has
// next is which declaration to change, and a bare "duplicate" sends them to
// re-derive it from the render.
func collapseIdentical(objects []release.ShapeObject) ([]release.ShapeObject, error) {
	seen := map[release.ObjectKey]release.ShapeObject{}
	out := make([]release.ShapeObject, 0, len(objects))
	for _, obj := range objects {
		key := obj.Key()
		prior, duplicate := seen[key]
		if !duplicate {
			seen[key] = obj
			out = append(out, obj)
			continue
		}
		if prior.Hash == obj.Hash {
			// Same bytes: one apply, one object, named once.
			continue
		}
		return nil, fmt.Errorf(
			"%w: %s is declared twice with different content (%s vs %s) — a deploy applies both and the last one wins, so one declaration silently overwrites the other; make them identical or remove one",
			release.ErrInvalid, key, prior.Hash, obj.Hash)
	}
	return out, nil
}
