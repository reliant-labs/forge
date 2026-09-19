package deployartifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/reliant-labs/forge/internal/statefile"
)

// Caching by digest — the property that makes it sound.
//
// A DIGEST NAMES BYTES. That single fact is what lets this cache skip
// every freshness check a normal HTTP cache needs: if the tag still
// resolves to a digest already on disk, the cached bytes ARE the current
// bytes, necessarily, with no TTL to tune and no staleness window. There
// is nothing to revalidate because content-addressed content cannot
// change under its name.
//
// So the hot path is exactly ONE round trip:
//
//	Resolve(ref)  →  descriptor (an HTTP HEAD against the manifest)
//	digest already unpacked?  →  return it, no pull
//
// and a pull happens only on a digest this machine has never seen. A
// reconcile loop polling an unchanged environment therefore costs one
// HEAD per interval rather than a full artifact download, which is the
// difference between a loop that can run every thirty seconds and one
// that cannot.
//
// A reference that is ITSELF a digest (`repo@sha256:…`) still goes
// through Resolve. It could be short-circuited, and deliberately is not:
// the resolve also proves the artifact is still PRESENT in the registry,
// and a loop that silently converged an environment onto a
// locally-cached artifact that had been deleted upstream would be
// reporting a desired state nobody could reproduce.

// Resolver is the narrow slice of a registry this package needs,
// DECLARED AT THE CONSUMER rather than imported from oras — one method
// for the HEAD and one for the pull. Anything that satisfies it works:
// oras.land/oras-go/v2's remote.Repository does, and so does the
// in-memory double the tests use.
//
// Two methods, not oras's full Target: the larger the interface, the
// weaker the abstraction, and this package needs to resolve a reference
// and read a blob. Nothing else.
type Resolver interface {
	// Resolve maps a reference (tag or digest) to its descriptor. This
	// is the HEAD — it must not transfer the content.
	Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error)
	// Fetch reads the content a descriptor names.
	Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error)
}

// CacheDirRel is where unpacked artifacts live, under the same .forge/
// tree as every other piece of forge runtime state (so the single
// existing .forge/ gitignore rule already covers it).
const CacheDirRel = statefile.DirRel + "/artifacts"

// Cache is a digest-keyed store of unpacked artifacts on local disk.
//
// The zero value is not usable; construct with NewCache.
type Cache struct {
	// root is the absolute cache directory.
	root string
	// mu serialises unpacks so two concurrent reconciles of the same
	// digest do not both write the same tree. The check-then-unpack
	// sequence is not atomic on its own, and O_EXCL inside Unpack turns
	// the race into a hard error rather than a silent interleave — which
	// would be correct but would fail a loop that did nothing wrong.
	mu sync.Mutex
}

// NewCache returns a Cache rooted under the project directory.
func NewCache(projectDir string) (*Cache, error) {
	if projectDir == "" {
		projectDir = "."
	}
	root, err := filepath.Abs(filepath.Join(projectDir, CacheDirRel))
	if err != nil {
		return nil, fmt.Errorf("resolve artifact cache dir: %w", err)
	}
	return &Cache{root: root}, nil
}

// Stats records what one Fetch actually did, so a caller (and a test) can
// tell a cache hit from a pull WITHOUT inferring it from timing.
//
// This exists because "an unchanged digest must cost one HEAD, not a full
// pull" is a claim that is invisible from the outside: both paths return
// the same Artifact. A test asserting the hit path by measuring duration
// would be the flaky-and-vacuous shape this project has been bitten by
// three times — it would pass against an implementation that pulled every
// time, on a fast enough machine. Recording the decision makes the
// property checkable rather than believed.
type Stats struct {
	// Resolved is true when the reference was resolved (the HEAD).
	// Always true on a successful fetch.
	Resolved bool
	// Pulled is true when the content was transferred. FALSE ON A CACHE
	// HIT — that is the property worth asserting.
	Pulled bool
	// Digest is the resolved manifest digest.
	Digest string
}

// pathFor returns the on-disk directory for one digest.
//
// The digest is split on its ':' so the tree is
// `artifacts/sha256/<hex>/` rather than a single directory holding a
// colon in every name — colons are legal on POSIX and a nuisance
// elsewhere. SafeSegment is applied to both halves even though a
// validated digest cannot contain a separator, because this path is
// composed from a value that arrived over the network and the cost of
// the guard is nothing.
func (c *Cache) pathFor(digest string) (string, error) {
	if !canonicalDigest.MatchString(digest) {
		return "", fmt.Errorf("%w: refusing to cache under non-canonical digest %q",
			ErrInvalidArtifact, digest)
	}
	algo, hex, ok := strings.Cut(digest, ":")
	if !ok {
		return "", fmt.Errorf("%w: digest %q has no algorithm prefix", ErrInvalidArtifact, digest)
	}
	return filepath.Join(c.root, statefile.SafeSegment(algo), statefile.SafeSegment(hex)), nil
}

// Fetch resolves ref, returns the cached artifact when its digest is
// already unpacked, and otherwise pulls, unpacks and caches it.
//
// The returned Stats reports which of those two happened.
func (c *Cache) Fetch(ctx context.Context, res Resolver, ref string) (Artifact, Stats, error) {
	var stats Stats

	// The HEAD. One round trip, and on the hot path the ONLY one.
	desc, err := res.Resolve(ctx, ref)
	if err != nil {
		return Artifact{}, stats, fmt.Errorf("resolve artifact %q: %w", ref, err)
	}
	stats.Resolved = true
	digest := desc.Digest.String()
	stats.Digest = digest

	dir, perr := c.pathFor(digest)
	if perr != nil {
		return Artifact{}, stats, perr
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if a, ok, herr := c.readCached(dir, digest); herr != nil {
		return Artifact{}, stats, herr
	} else if ok {
		// CACHE HIT. No Fetch call was made — this is the property
		// Stats.Pulled exists to make observable.
		return a, stats, nil
	}

	rc, ferr := res.Fetch(ctx, desc)
	if ferr != nil {
		return Artifact{}, stats, fmt.Errorf("fetch artifact %s: %w", digest, ferr)
	}
	defer func() { _ = rc.Close() }()
	stats.Pulled = true

	// Unpack into a TEMPORARY sibling and rename into place, so an
	// interrupted pull cannot leave a half-written tree under a digest
	// that the next run would then treat as a complete cache hit. A
	// partial tree keyed by a valid digest is worse than no cache: the
	// digest would be a promise about bytes the directory does not hold.
	staging, terr := os.MkdirTemp(filepath.Dir(dir), ".partial-*")
	if terr != nil {
		if mkerr := os.MkdirAll(filepath.Dir(dir), unpackDirMode); mkerr != nil {
			return Artifact{}, stats, fmt.Errorf("create cache dir: %w", mkerr)
		}
		staging, terr = os.MkdirTemp(filepath.Dir(dir), ".partial-*")
		if terr != nil {
			return Artifact{}, stats, fmt.Errorf("create staging dir: %w", terr)
		}
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if _, uerr := Unpack(rc, staging); uerr != nil {
		return Artifact{}, stats, uerr
	}
	a, derr := ReadArtifactDoc(staging)
	if derr != nil {
		return Artifact{}, stats, derr
	}
	a.Digest = digest

	if rerr := os.Rename(staging, dir); rerr != nil {
		// A concurrent process may have won the race and populated the
		// directory with the SAME digest — which by definition holds the
		// same bytes, so its copy is as good as ours.
		if _, serr := os.Stat(dir); serr == nil {
			return a, stats, nil
		}
		return Artifact{}, stats, fmt.Errorf("publish artifact %s into cache: %w", digest, rerr)
	}
	return a, stats, nil
}

// readCached returns the artifact already unpacked under dir, if any.
//
// A missing directory is a clean miss. A directory that EXISTS but whose
// document is unreadable is an error rather than a miss: silently
// re-pulling over a corrupt cache entry would hide the corruption
// forever, and this is the one place that would notice it.
func (c *Cache) readCached(dir, digest string) (Artifact, bool, error) {
	if _, err := os.Stat(filepath.Join(dir, ArtifactDocName)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Artifact{}, false, nil
		}
		return Artifact{}, false, fmt.Errorf("stat cached artifact %s: %w", digest, err)
	}
	a, err := ReadArtifactDoc(dir)
	if err != nil {
		return Artifact{}, false, fmt.Errorf("cached artifact %s is unreadable (remove %s to re-pull): %w",
			digest, dir, err)
	}
	a.Digest = digest
	return a, true, nil
}
