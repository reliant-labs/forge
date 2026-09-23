package deployartifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// The registry-backed Resolver.
//
// oras.land/oras-go/v2 was already in forge's dependency tree before this
// package existed; this is the first direct consumer. It is used for
// exactly two operations — Resolve (the HEAD) and Fetch (the pull) —
// which is why Resolver is a two-method interface declared at the
// consumer rather than oras's own Target. A registry client that grew
// into this package's public surface would make every future caller
// depend on oras's types to do anything.

// RemoteResolver wraps an oras remote.Repository as a [Resolver].
//
// The manifest fetch is what Resolve returns a descriptor for; Fetch on
// that descriptor reads the manifest, and FetchLayer reads the single
// layer blob an artifact carries. Keeping the manifest step visible
// rather than hiding it behind a one-call "give me the bytes" is what
// lets the cache do its HEAD-only hot path: a caller that only wants the
// digest never asks for a blob.
type RemoteResolver struct {
	repo *remote.Repository
}

// NewRemoteResolver builds a Resolver for a registry reference of the
// form `registry/repo` (no tag — the tag or digest is passed to Resolve).
//
// Credentials come from the ambient docker credential chain via oras's
// default auth client, and the transport retries on the registry's own
// throttling responses. Both are oras defaults rather than forge policy:
// a reconcile loop that polls a registry every interval WILL eventually
// be rate-limited, and a client that treats a 429 as a hard failure would
// report the environment as unobservable for the duration.
func NewRemoteResolver(ref string) (*RemoteResolver, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("deployartifact: empty registry reference")
	}
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, fmt.Errorf("deployartifact: parse registry reference %q: %w", ref, err)
	}
	repo.Client = &auth.Client{
		Client:     retry.DefaultClient,
		Cache:      auth.NewCache(),
		Credential: auth.StaticCredential("", auth.EmptyCredential),
	}
	return &RemoteResolver{repo: repo}, nil
}

// Resolve performs the HEAD against the manifest and returns its
// descriptor. It transfers no content.
func (r *RemoteResolver) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	return r.repo.Resolve(ctx, reference)
}

// Fetch reads the content a descriptor names, VERIFIED against that
// descriptor's digest and size.
//
// content.VerifyReader is not optional decoration. A registry response is
// bytes from outside; without the verification wrapper a corrupted or
// substituted body would be unpacked as though it were the artifact the
// digest promised, which would defeat the entire reason this design is
// content-addressed. The wrapper fails the read at the point the hash
// diverges rather than after the bytes have landed on disk.
func (r *RemoteResolver) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	rc, err := r.repo.Fetch(ctx, target)
	if err != nil {
		return nil, err
	}
	return &verifyingReadCloser{
		verifier: content.NewVerifyReader(rc, target),
		closer:   rc,
	}, nil
}

// verifyingReadCloser makes the verification TERMINAL: Close only
// succeeds if the full content hashed to the descriptor's digest.
//
// This is the half that is easy to get wrong. content.VerifyReader only
// checks the hash when Verify() is called, so a consumer that read the
// stream and closed it would get unverified bytes and no error — the
// verification would be present in the code and absent in effect.
// Driving Verify from Close means a caller cannot skip it without
// skipping Close, which nothing does.
type verifyingReadCloser struct {
	verifier *content.VerifyReader
	closer   io.Closer
}

func (v *verifyingReadCloser) Read(p []byte) (int, error) { return v.verifier.Read(p) }

func (v *verifyingReadCloser) Close() error {
	if err := v.verifier.Verify(); err != nil {
		_ = v.closer.Close()
		return fmt.Errorf("artifact content failed digest verification: %w", err)
	}
	return v.closer.Close()
}
