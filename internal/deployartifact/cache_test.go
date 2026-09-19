package deployartifact

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeRegistry is an in-memory Resolver that COUNTS what it was asked to
// do.
//
// The counts are the whole point. "An unchanged digest must cost one
// HEAD, not a full pull" is invisible from the outside — both paths
// return the same Artifact — so a test that tried to demonstrate it by
// measuring elapsed time would pass against an implementation that pulled
// every single time, on a machine fast enough. Counting the calls makes
// the property CHECKABLE rather than believed, which is the difference
// this project's three prior vacuous-test incidents turned on.
type fakeRegistry struct {
	mu sync.Mutex
	// blob is the archive bytes served for the one artifact.
	blob []byte
	// desc is what Resolve returns.
	desc ocispec.Descriptor
	// resolves and fetches count the two operations.
	resolves int
	fetches  int
	// resolveErr / fetchErr, when set, fail that operation.
	resolveErr error
	fetchErr   error
}

func (f *fakeRegistry) Resolve(_ context.Context, _ string) (ocispec.Descriptor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if f.resolveErr != nil {
		return ocispec.Descriptor{}, f.resolveErr
	}
	return f.desc, nil
}

func (f *fakeRegistry) Fetch(_ context.Context, _ ocispec.Descriptor) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return io.NopCloser(bytes.NewReader(f.blob)), nil
}

func (f *fakeRegistry) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolves, f.fetches
}

// newFakeRegistry serves a well-formed artifact whose descriptor digest
// is the REAL sha256 of the bytes served. Using the true hash rather than
// an arbitrary string matters: the cache keys directories on it and
// validates its canonical form, so a fabricated digest would exercise a
// shape a registry cannot produce.
func newFakeRegistry(t *testing.T, doc string) *fakeRegistry {
	t.Helper()
	blob := buildTarGz(t, []tarEntry{
		{name: ArtifactDocName, body: doc},
		{name: "manifests/deployment.yaml", body: "kind: Deployment\n"},
	})
	sum := sha256.Sum256(blob)
	return &fakeRegistry{
		blob: blob,
		desc: ocispec.Descriptor{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    digest.Digest("sha256:" + hex.EncodeToString(sum[:])),
			Size:      int64(len(blob)),
		},
	}
}

// TestCache_SecondFetchOfAnUnchangedDigestCostsOneHEAD is the caching
// property, stated as the call counts it reduces to.
//
// First fetch:  1 resolve + 1 fetch (cold).
// Second fetch: 1 MORE resolve, and NO more fetches.
//
// The second assertion is the one that matters and the one an
// implementation without a cache fails.
func TestCache_SecondFetchOfAnUnchangedDigestCostsOneHEAD(t *testing.T) {
	dir := t.TempDir()
	reg := newFakeRegistry(t, validDoc(t))
	cache, err := NewCache(dir)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	ctx := context.Background()

	first, stats1, err := cache.Fetch(ctx, reg, "ghcr.io/x/art:v1")
	if err != nil {
		t.Fatalf("cold fetch: %v", err)
	}
	if !stats1.Pulled {
		t.Error("the COLD fetch reports Pulled=false — if nothing was pulled, the warm-path " +
			"assertion below proves nothing")
	}
	if r, f := reg.counts(); r != 1 || f != 1 {
		t.Fatalf("cold fetch made %d resolves and %d fetches, want 1 and 1", r, f)
	}

	second, stats2, err := cache.Fetch(ctx, reg, "ghcr.io/x/art:v1")
	if err != nil {
		t.Fatalf("warm fetch: %v", err)
	}
	if stats2.Pulled {
		t.Error("the WARM fetch pulled content for a digest already unpacked")
	}
	r, f := reg.counts()
	if r != 2 {
		t.Errorf("warm fetch made %d total resolves, want 2 — the HEAD still happens, because it "+
			"is what proves the tag has not moved AND that the artifact still exists upstream", r)
	}
	if f != 1 {
		t.Fatalf("warm fetch made %d total fetches, want 1 — an unchanged digest must cost one "+
			"HEAD, not a full pull", f)
	}

	if first.Digest != second.Digest || first.Release != second.Release {
		t.Errorf("cached artifact differs from the pulled one: %+v vs %+v", first, second)
	}
	if len(second.Items) != len(first.Items) {
		t.Errorf("cached item count %d != pulled %d", len(second.Items), len(first.Items))
	}
}

// TestCache_ChangedDigestPulls is the other half. Without it, the test
// above is satisfiable by a cache that NEVER pulls again — returning
// stale desired state forever, which is worse than no cache.
func TestCache_ChangedDigestPulls(t *testing.T) {
	dir := t.TempDir()
	cache, _ := NewCache(dir)
	ctx := context.Background()

	regA := newFakeRegistry(t, validDoc(t))
	if _, _, err := cache.Fetch(ctx, regA, "ref"); err != nil {
		t.Fatalf("first: %v", err)
	}

	// A different document ⇒ different bytes ⇒ different digest.
	otherDoc := strings.Replace(validDoc(t), "v1.4.0", "v1.5.0", 1)
	regB := newFakeRegistry(t, otherDoc)
	if regB.desc.Digest == regA.desc.Digest {
		t.Fatal("the two fixtures hash identically — the test would pass for the wrong reason")
	}

	art, stats, err := cache.Fetch(ctx, regB, "ref")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !stats.Pulled {
		t.Fatal("a CHANGED digest did not pull — the cache is serving stale desired state")
	}
	if art.Release != "v1.5.0" {
		t.Errorf("release = %q, want the new artifact's v1.5.0", art.Release)
	}
}

// TestCache_ReportsTheResolvedDigest pins that the artifact carries its
// OWN content-addressed identity, taken from the transport rather than
// from the document. A document that named its own digest would be
// asserting something only the transport can verify.
func TestCache_ReportsTheResolvedDigest(t *testing.T) {
	reg := newFakeRegistry(t, validDoc(t))
	cache, _ := NewCache(t.TempDir())
	art, stats, err := cache.Fetch(context.Background(), reg, "ref")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := reg.desc.Digest.String()
	if art.Digest != want {
		t.Errorf("Artifact.Digest = %q, want the resolved %q", art.Digest, want)
	}
	if stats.Digest != want {
		t.Errorf("Stats.Digest = %q, want %q", stats.Digest, want)
	}
}

// TestCache_InterruptedPullLeavesNoCacheEntry.
//
// A half-written tree keyed by a VALID digest is worse than no cache: the
// digest is a promise about bytes the directory does not hold, and the
// next run would serve it as a hit. The unpack here fails on a hostile
// entry partway through, and the assertion is that no directory survives
// under that digest.
func TestCache_InterruptedPullLeavesNoCacheEntry(t *testing.T) {
	dir := t.TempDir()
	// An archive whose first entry is fine and whose second is a
	// traversal: unpack gets partway, then refuses.
	blob := buildTarGz(t, []tarEntry{
		{name: "manifests/ok.yaml", body: "kind: ConfigMap\n"},
		{name: "../escape.txt", body: "nope"},
	})
	sum := sha256.Sum256(blob)
	reg := &fakeRegistry{
		blob: blob,
		desc: ocispec.Descriptor{
			Digest: digest.Digest("sha256:" + hex.EncodeToString(sum[:])),
			Size:   int64(len(blob)),
		},
	}
	cache, _ := NewCache(dir)

	if _, _, err := cache.Fetch(context.Background(), reg, "ref"); !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive", err)
	}

	cacheRoot := filepath.Join(dir, CacheDirRel)
	var leaked []string
	_ = filepath.Walk(cacheRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // a missing cache root is the pass condition
		}
		leaked = append(leaked, path)
		return nil
	})
	if len(leaked) > 0 {
		t.Errorf("a failed pull left files in the cache: %v — a partial tree under a valid digest "+
			"would be served as a hit on the next run", leaked)
	}
}

// TestCache_RefusesNonCanonicalDigest. The cache composes a filesystem
// path from a value that arrived over the network, so the canonical-form
// check is a path-safety guard as much as a correctness one.
func TestCache_RefusesNonCanonicalDigest(t *testing.T) {
	reg := newFakeRegistry(t, validDoc(t))
	reg.desc.Digest = digest.Digest("sha256:../../etc/passwd")
	cache, _ := NewCache(t.TempDir())
	_, _, err := cache.Fetch(context.Background(), reg, "ref")
	if !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("error = %v, want ErrInvalidArtifact for a non-canonical digest", err)
	}
	if f := reg.fetches; f != 0 {
		t.Errorf("content was fetched (%d×) before the digest was validated", f)
	}
}

// TestCache_InvalidDocumentIsRefusedNotCached. An artifact whose document
// violates the 00075 rules must not land in the cache, or every
// subsequent run would serve the invalid desired state as a clean hit.
func TestCache_InvalidDocumentIsRefusedNotCached(t *testing.T) {
	dir := t.TempDir()
	// An oci item with no digest — the rule the migration argues hardest for.
	reg := newFakeRegistry(t, `{"release":"v1","items":{"api":{"name":"api","kind":"oci"}}}`)
	cache, _ := NewCache(dir)

	if _, _, err := cache.Fetch(context.Background(), reg, "ref"); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("error = %v, want ErrInvalidArtifact", err)
	}

	// And a second attempt must still pull + still refuse, not hit a cache.
	before := reg.fetches
	if _, _, err := cache.Fetch(context.Background(), reg, "ref"); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("second attempt error = %v, want ErrInvalidArtifact", err)
	}
	if reg.fetches == before {
		t.Error("the second attempt did not re-fetch — an invalid artifact was cached")
	}
}

// TestCache_ResolveFailureDoesNotFetch: a registry forge cannot reach
// must not produce a content request, and must not be mistakable for a
// hit.
func TestCache_ResolveFailureDoesNotFetch(t *testing.T) {
	reg := newFakeRegistry(t, validDoc(t))
	reg.resolveErr = errors.New("dial tcp: i/o timeout")
	cache, _ := NewCache(t.TempDir())

	_, stats, err := cache.Fetch(context.Background(), reg, "ref")
	if err == nil {
		t.Fatal("an unreachable registry returned no error")
	}
	if stats.Resolved || stats.Pulled {
		t.Errorf("stats = %+v, want neither resolved nor pulled", stats)
	}
	if _, f := reg.counts(); f != 0 {
		t.Errorf("content was fetched %d× despite the resolve failing", f)
	}
}

// TestUnpackedArchiveRoundTripsThroughTheCache is the round trip the
// brief asks for, end to end: a REAL gzipped tar goes in at the registry
// boundary, and a validated Artifact with typed items comes out — with no
// hand-built Artifact literal anywhere on the path. A change to the
// document's field names, the tar layout, or the validation rules breaks
// this; a struct-literal test would keep passing against the stale shape.
func TestUnpackedArchiveRoundTripsThroughTheCache(t *testing.T) {
	reg := newFakeRegistry(t, validDoc(t))
	cache, _ := NewCache(t.TempDir())

	art, _, err := cache.Fetch(context.Background(), reg, "ghcr.io/x/art:v1.4.0")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if art.Release != "v1.4.0" || art.Env != "prod" {
		t.Errorf("artifact = %+v", art)
	}

	api, ok := art.Items["api"]
	if !ok {
		t.Fatal("the oci item did not survive the round trip")
	}
	d, pinned := api.PinnedDigest()
	if !pinned || d != "sha256:"+strings.Repeat("ab", 32) {
		t.Errorf("api PinnedDigest = (%q, %v)", d, pinned)
	}

	npm, ok := art.Items["web-runtime"]
	if !ok {
		t.Fatal("the npm item did not survive the round trip")
	}
	if npm.EffectiveKind() != KindNPM || npm.Version != "0.3.1" {
		t.Errorf("npm item = %+v", npm)
	}
	if _, pinned := npm.PinnedDigest(); pinned {
		t.Error("the npm item reports a pinnable digest after a round trip")
	}
}

// TestKindVocabularyIsTheLedgerSpelling pins the tokens against the
// values migration 00075 constrains its kind column to and that forge's
// own ReleaseArtifact uses. Three spellings of one vocabulary is how a
// producer and a consumer drift into disagreeing about what an artifact
// IS — and a rename here would be invisible until a real release failed
// to deploy.
func TestKindVocabularyIsTheLedgerSpelling(t *testing.T) {
	for got, want := range map[string]string{
		KindOCI:      "oci",
		KindNPM:      "npm",
		KindGoModule: "gomod",
		KindFile:     "file",
	} {
		if got != want {
			t.Errorf("kind constant = %q, want %q (the ledger + migration 00075 spelling)", got, want)
		}
	}
	// And the default, which both sides express as
	// COALESCE(NULLIF(kind,''),'oci') / EffectiveKind().
	if (Item{}).EffectiveKind() != KindOCI {
		t.Error("an empty kind must mean oci, matching ReleaseArtifact.EffectiveKind")
	}
}

// tarEntryTypesAreFromArchiveTar keeps the test helper honest: if
// archive/tar's constants were ever shadowed locally, the hostile
// fixtures would stop being hostile.
var _ = tar.TypeSymlink
