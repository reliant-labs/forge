package deploytarget

// A HOSTED StaticSite release is an OCI artifact.
//
// forge assembles the site exactly as the self-hosted executor does (the
// shared StagePlan), packs the tree into ONE deterministic tar.gz layer, and
// pushes it to the org's registry subtree — the same `image_push_base` the
// org already pushes backend images to, under the reserved `static.v1`
// segment. The manifest digest IS the release: the ledger records it like any
// image, promotion binds it, and the StaticSite spec pins it as liveDigest.
// The control plane's operator derives the SAME repository from the CR's
// platform-stamped org label, pulls the digest, and writes it into
// sites/<org>/<env>/<site>/releases/<digest>/ before syncing live/.
//
// Why this and not a signed upload URL into the platform bucket: the org
// then never holds any bucket credential at all (the bucket's only writer
// stays the operator), there is no second credential type or RPC, and a
// release is content-addressed and immutable by construction instead of "a
// prefix someone wrote to". See .forge/scratch/unify/reports/STATIC.md.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	// StaticSiteArtifactType is the OCI artifactType of a site release.
	StaticSiteArtifactType = "application/vnd.forge.staticsite.v1"
	// StaticSiteLayerMediaType is its single layer: the assembled tree.
	StaticSiteLayerMediaType = "application/vnd.forge.staticsite.layer.v1.tar+gzip"
	// StaticSiteRepositorySegment is the path segment site releases live
	// under inside an org's registry subtree. THE DOT IS LOAD-BEARING: an
	// RFC-1123 build name cannot contain one, so no backend image an org
	// builds can collide with a site release (the control plane's
	// ociregistry.ConfigNamespace uses the same property).
	StaticSiteRepositorySegment = "static.v1"
)

// HostedStaticRepository is the repository a site's releases are pushed to
// under a registry push base (the org subtree).
func HostedStaticRepository(pushBase, site string) string {
	return strings.TrimSuffix(strings.TrimSpace(pushBase), "/") + "/" + StaticSiteRepositorySegment + "/" + site
}

// BuildStaticSiteTree builds and assembles one frontend with the shared
// StagePlan and returns the assembled directory — the exact tree the
// self-hosted executor would upload.
func BuildStaticSiteTree(ctx context.Context, projectDir string, fe StaticSiteFrontend) (string, error) {
	p := StaticSiteProvider{ProjectDir: projectDir}
	plan, err := p.buildPlan(fe)
	if err != nil {
		return "", err
	}
	if err := runStagePlan(ctx, p.runner(), plan.Stage); err != nil {
		return "", err
	}
	return plan.Stage.StagingDir, nil
}

// PackStaticSiteTree packs an assembled tree into a DETERMINISTIC tar.gz:
// entries sorted, zero timestamps and owners, fixed modes, no gzip header
// time. Identical trees therefore produce identical bytes, so re-pushing
// unchanged content yields the same digest and an idempotent release cut.
//
// Only regular files and directories are packed. A symlink is REFUSED rather
// than followed or preserved: followed, it would ship whatever it points at on
// the build machine; preserved, it would ask the platform to materialize a
// link it must never write into a shared bucket.
func PackStaticSiteTree(dir string) ([]byte, error) {
	type entry struct{ rel, path string }
	var files []entry
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == dir || info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file (mode %s); a static site release carries only files", filepath.ToSlash(rel), info.Mode())
		}
		files = append(files, entry{rel: filepath.ToSlash(rel), path: path})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", dir, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("pack %s: the assembled site is empty; refusing to publish a release that would serve nothing", dir)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		data, rerr := os.ReadFile(f.path)
		if rerr != nil {
			return nil, fmt.Errorf("pack %s: %w", f.rel, rerr)
		}
		hdr := &tar.Header{Name: f.rel, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
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

// fixedCreated pins the manifest's created annotation so the manifest — and
// therefore the release digest — depends on the content alone.
const fixedCreated = "1970-01-01T00:00:00Z"

// PushStaticSiteArtifact pushes a packed site to repository and returns the
// manifest digest. Credentials come from the ambient docker credential store
// — the same one `docker push` of a backend image to the same push base uses.
func PushStaticSiteArtifact(ctx context.Context, repository string, layer []byte) (string, error) {
	repo, err := remote.NewRepository(repository)
	if err != nil {
		return "", fmt.Errorf("static site repository %q: %w", repository, err)
	}
	repo.PlainHTTP = plainHTTPRegistry(repo.Reference.Registry)
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return "", fmt.Errorf("read docker credentials: %w", err)
	}
	repo.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache(), Credential: credentials.Credential(store)}
	return pushStaticSite(ctx, repo, layer)
}

// pushStaticSite is the transport-agnostic half, so it runs against any
// oras target (an in-memory store in tests). The manifest is composed here
// rather than by oras.PackManifest so its digest is a pure function of the
// layer: a re-push of an unchanged site returns the same release digest even
// when the target answers "already exists".
func pushStaticSite(ctx context.Context, target content.Pusher, layer []byte) (string, error) {
	layerDesc := content.NewDescriptorFromBytes(StaticSiteLayerMediaType, layer)
	manifest := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: StaticSiteArtifactType,
		Config:       ocispec.DescriptorEmptyJSON,
		Layers:       []ocispec.Descriptor{layerDesc},
		Annotations:  map[string]string{ocispec.AnnotationCreated: fixedCreated},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	manifestDesc := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, raw)
	for _, blob := range []struct {
		desc ocispec.Descriptor
		data []byte
	}{{layerDesc, layer}, {ocispec.DescriptorEmptyJSON, ocispec.DescriptorEmptyJSON.Data}, {manifestDesc, raw}} {
		if err := target.Push(ctx, blob.desc, bytes.NewReader(blob.data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return "", fmt.Errorf("push %s: %w", blob.desc.MediaType, err)
		}
	}
	return manifestDesc.Digest.String(), nil
}

// plainHTTPRegistry reports the registries reached over plain HTTP: the
// loopback and *.localhost names a local k3d registry is published under.
func plainHTTPRegistry(host string) bool {
	h := host
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasPrefix(h, "127.") || h == "host.k3d.internal"
}
