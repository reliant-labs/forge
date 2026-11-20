package sbomoci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/reliant-labs/forge/internal/ledgerfile"
	"github.com/reliant-labs/forge/pkg/release"
)

// Where [Fetch] found an SBOM's bytes.
const (
	// SourceLedger is the machine ledger's content-addressed SBOM store.
	SourceLedger = "ledger"
	// SourceReferrer is the OCI referrer beside the artifact in its registry.
	SourceReferrer = "referrer"
)

// ErrNoSBOM is an artifact whose release record names no SBOM bytes: none
// was recorded, or its SBOMRef says unavailable.
var ErrNoSBOM = errors.New("the release records no sbom for this artifact")

// ErrSBOMUnavailable is a recorded SBOM that no copy could be read back
// from: not in the ledger, and no referrer holding those exact bytes.
var ErrSBOMUnavailable = errors.New("no copy of the recorded sbom could be read")

// maxSBOMBytes bounds what a referrer layer may claim before it is read. A
// CycloneDX document of a large image is a few MB; this is far past any real
// one and still refuses a registry that names an absurd size.
const maxSBOMBytes = 256 << 20

// maxManifestBytes bounds a referrer manifest read.
const maxManifestBytes = 4 << 20

// Fetch returns the bytes of artifact's recorded SBOM and where they came
// from ([SourceLedger] or [SourceReferrer]), verified against
// artifact.SBOM.Digest whatever the source.
//
// The release record is the index; a registry is never asked "what is
// attached" before the ledger is asked for the digest the record names:
//
//  1. the machine ledger copy, by digest (store may be nil: skipped);
//  2. the referrer the record names (SBOMRef.Referrer);
//  3. else subject's referrers of type [ArtifactType], filtered to the one
//     holding that digest (subject is `<repository>@sha256:<hex>`; empty
//     skips discovery).
//
// Bytes read from a referrer are cached into store, so the next read — and
// every scan — needs no network. A ledger copy that no longer matches its
// digest is passed over and repaired by that cache write.
func Fetch(ctx context.Context, store *ledgerfile.Store, artifact release.Artifact, subject string) ([]byte, string, error) {
	return FetchWith(ctx, store, artifact, subject, RegistryOptions{})
}

// FetchWith is [Fetch] with explicit registry options.
func FetchWith(ctx context.Context, store *ledgerfile.Store, artifact release.Artifact, subject string, opts RegistryOptions) ([]byte, string, error) {
	ref := artifact.SBOM
	if !ref.HasSBOM() {
		return nil, "", ErrNoSBOM
	}
	want, err := digest.Parse(ref.Digest)
	if err != nil || want.Algorithm() != digest.SHA256 {
		return nil, "", fmt.Errorf("recorded sbom digest %q is not a sha256 digest", ref.Digest)
	}

	var misses []error
	if store != nil {
		data, err := store.GetSBOM(ref.Digest)
		if err == nil {
			return data, SourceLedger, nil
		}
		misses = append(misses, fmt.Errorf("ledger: %w", err))
	}

	data, err := fetchFromRegistry(ctx, want, ref.Referrer, subject, opts)
	if err != nil {
		misses = append(misses, err)
		return nil, "", fmt.Errorf("%w (%s): %w", ErrSBOMUnavailable, want, errors.Join(misses...))
	}
	if store != nil {
		// A failed cache write loses nothing: the bytes are verified and in
		// hand, and the referrer still holds them.
		_, _ = store.PutSBOM(data)
	}
	return data, SourceReferrer, nil
}

// fetchFromRegistry reads the SBOM with digest want from the named referrer,
// else by discovering subject's referrers.
func fetchFromRegistry(ctx context.Context, want digest.Digest, referrer, subject string, opts RegistryOptions) ([]byte, error) {
	var misses []error
	if referrer != "" {
		data, err := fetchReferrer(ctx, want, referrer, opts)
		if err == nil {
			return data, nil
		}
		misses = append(misses, fmt.Errorf("referrer %s: %w", referrer, err))
	}
	if subject != "" {
		data, err := discover(ctx, want, subject, opts)
		if err == nil {
			return data, nil
		}
		misses = append(misses, fmt.Errorf("referrers of %s: %w", subject, err))
	}
	if len(misses) == 0 {
		return nil, errors.New("the record names no referrer and no subject was given to look one up by")
	}
	return nil, errors.Join(misses...)
}

// fetchReferrer reads `<repository>@sha256:<hex>`, a referrer manifest, and
// returns its layer holding want.
func fetchReferrer(ctx context.Context, want digest.Digest, referrer string, opts RegistryOptions) ([]byte, error) {
	ref, d, err := parseSubject(referrer)
	if err != nil {
		return nil, err
	}
	repo, err := openRepository(ref.Registry+"/"+ref.Repository, opts)
	if err != nil {
		return nil, err
	}
	desc, err := repo.Resolve(ctx, d.String())
	if err != nil {
		return nil, err
	}
	return sbomFromManifest(ctx, repo, desc, want)
}

// discover finds, among subject's SBOM referrers, the one holding want: by
// its AnnotationSBOMDigest first, then (a referrer another tool attached,
// which carries no such annotation) by reading each manifest's layers.
func discover(ctx context.Context, want digest.Digest, subject string, opts RegistryOptions) ([]byte, error) {
	ref, d, err := parseSubject(subject)
	if err != nil {
		return nil, err
	}
	repo, err := openRepository(ref.Registry+"/"+ref.Repository, opts)
	if err != nil {
		return nil, err
	}
	subjectDesc, err := resolveSubject(ctx, repo, d)
	if err != nil {
		return nil, err
	}
	listed, err := listReferrers(ctx, repo, subjectDesc)
	if err != nil {
		return nil, err
	}
	var annotated, others []ocispec.Descriptor
	for _, r := range listed {
		switch r.Annotations[AnnotationSBOMDigest] {
		case want.String():
			annotated = append(annotated, r)
		case "":
			others = append(others, r)
		}
	}
	var misses []error
	for _, r := range append(annotated, others...) {
		data, err := sbomFromManifest(ctx, repo, r, want)
		if err == nil {
			return data, nil
		}
		misses = append(misses, fmt.Errorf("%s: %w", r.Digest, err))
	}
	if len(misses) == 0 {
		return nil, fmt.Errorf("none of its %d sbom referrer(s) holds %s", len(listed), want)
	}
	return nil, errors.Join(misses...)
}

// sbomFromManifest reads the referrer manifest desc names and returns the
// bytes of its layer with digest want, verified by content.ReadAll against
// the layer descriptor (digest and size) and then against want itself.
func sbomFromManifest(ctx context.Context, repo *remote.Repository, desc ocispec.Descriptor, want digest.Digest) ([]byte, error) {
	if desc.Size > maxManifestBytes {
		return nil, fmt.Errorf("manifest %s is %d bytes, over the %d a referrer manifest may be", desc.Digest, desc.Size, maxManifestBytes)
	}
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	raw, err := content.ReadAll(rc, desc)
	_ = rc.Close()
	if err != nil {
		return nil, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest %s: %w", desc.Digest, err)
	}
	for _, layer := range manifest.Layers {
		if layer.Digest != want {
			continue
		}
		if layer.Size > maxSBOMBytes {
			return nil, fmt.Errorf("sbom layer %s is %d bytes, over the %d forge reads", layer.Digest, layer.Size, maxSBOMBytes)
		}
		lrc, err := repo.Fetch(ctx, layer)
		if err != nil {
			return nil, err
		}
		data, err := content.ReadAll(lrc, layer)
		_ = lrc.Close()
		if err != nil {
			return nil, err
		}
		if got := digest.FromBytes(data); got != want {
			return nil, fmt.Errorf("sbom layer read back as %s, want %s", got, want)
		}
		return data, nil
	}
	return nil, fmt.Errorf("manifest %s has no layer %s", desc.Digest, want)
}
