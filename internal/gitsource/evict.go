// Cache eviction for the machine-local source cache.
//
// # Why the cache needed a reclaimer at all
//
// Resolve writes one FULL clone per repo+ref (CacheKey), and the only
// removal anywhere in this package is the explicit Refresh. A project
// that bumps its pin every few days therefore accumulates one complete
// checkout per bump, forever. Measured on a developer machine:
// <UserCacheDir>/forge/sources held 16 clones of one repository totalling
// 9.2 GB, because each entry also carries whatever the frontend install
// wrote into it (web/node_modules was 0.54 GB of a 0.67 GB entry).
//
// Nothing about that is a correctness bug — every entry is a valid,
// re-fetchable materialization of a reviewable pin — which is exactly why
// it went unnoticed until the disk filled.
//
// # Recording last use: why the metadata file's MTIME
//
// Eviction needs to know when an entry was last USED, which is not when
// it was fetched. Three candidates, and the choice matters:
//
//   - Rewriting a timestamp INTO .forge-source.json. Rejected. That file
//     is the completion marker: its presence is what makes an entry a
//     cache hit (see Resolve). Rewriting it on every hit puts the one
//     file that must never be half-written on the hot path, and a crash
//     mid-write would downgrade a complete 700 MB entry to a miss and
//     force a full re-clone. The cost of being wrong is far higher than
//     the information is worth.
//   - The entry DIRECTORY's mtime. Rejected. A directory's mtime tracks
//     changes to its own direct children, not reads, so it records the
//     fetch and not the use. It is also perturbed by anything that writes
//     into the tree — an `npm install` in a subdir — which conflates
//     "something wrote here" with "forge resolved this pin".
//   - The metadata file's MTIME, touched with os.Chtimes. CHOSEN.
//
// Chtimes writes no content, so an interrupted touch cannot corrupt the
// completion marker; the worst case is a missed touch, which costs one
// re-fetch of a re-fetchable pin. The file already exists on every
// complete entry, so caches that predate this code need no backfill:
// their mtime is the fetch time, which is a correct lower bound for last
// use. And there is no second marker file to keep in sync with the first.
//
// # Why an entry in use cannot be evicted
//
// The recency guard is the real protection, not the process scan. Resolve
// touches the metadata on every cache HIT, so any process currently
// building from an entry touched it moments ago and cannot be in a set
// whose youngest member is MaxAge (default 14 days) old. The open-file
// scan below is defense in depth for the pathological case — a process
// that resolved an entry and then held it open for longer than MaxAge —
// and when it is unavailable (no lsof, Windows) eviction proceeds on the
// recency guard alone rather than disabling itself entirely.
//
// Nothing persists a resolved cache path into a project's .forge/ state;
// the resolved directory is rewritten into an in-memory FrontendConfig
// and never serialized (see internal/cli/frontend_source.go and
// BuildState, which records image/tag/digest and no source paths). So
// there is no stored reference an eviction could invalidate.
package gitsource

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// DefaultEvictMaxAge is how long an entry may go unresolved before it
// becomes a candidate. Two weeks is long enough that a pin a developer
// returns to between sprints survives, and short enough that abandoned
// bumps do not accumulate for a quarter.
const DefaultEvictMaxAge = 14 * 24 * time.Hour

// DefaultEvictKeepPerSlug is how many of the most recently used entries
// for one repository are retained REGARDLESS of age.
//
// This is the floor that makes eviction safe for an idle project. A
// repository pinned to one ref and not built for a month has exactly one
// entry, and it is the entry the next build needs; evicting it on age
// alone would turn the first build after a holiday into a full re-clone.
// Two, not one, so that a developer flipping between a release pin and a
// main pin keeps both warm.
const DefaultEvictKeepPerSlug = 2

// EvictPolicy bounds what Evict is allowed to remove.
type EvictPolicy struct {
	// MaxAge is the unused age beyond which an entry is a candidate.
	// Zero means DefaultEvictMaxAge.
	MaxAge time.Duration

	// KeepPerSlug is the number of most-recently-used entries per
	// repository retained regardless of age. Zero means
	// DefaultEvictKeepPerSlug; it is never treated as "keep none",
	// because a policy that can empty the cache of a repository's only
	// pin is a policy nobody wants by accident.
	KeepPerSlug int

	// InUse reports whether a live process holds the entry directory as
	// its cwd or has a file open under it. nil installs the default
	// open-file scan (one lsof snapshot, taken once, only when there are
	// candidates). Tests inject a stub so they never shell out.
	InUse func(entry string) bool
}

func (p EvictPolicy) maxAge() time.Duration {
	if p.MaxAge <= 0 {
		return DefaultEvictMaxAge
	}
	return p.MaxAge
}

func (p EvictPolicy) keepPerSlug() int {
	if p.KeepPerSlug <= 0 {
		return DefaultEvictKeepPerSlug
	}
	return p.KeepPerSlug
}

// Eviction reports what Evict did, or would do under apply=false. The
// counts are returned rather than only printed so a test can assert on
// the decision instead of parsing prose.
type Eviction struct {
	// Removed names the cache entries evicted (or that would be).
	Removed []string
	// Bytes is the total size of Removed, as measured before removal.
	Bytes int64
	// Kept is the number of entries retained for any reason.
	Kept int
	// Held names entries a live process was using, which are always
	// retained. Reported separately from Kept's reasons because it is
	// the one retention a caller may want to re-run for later.
	Held []string
}

// Evict removes source-cache entries under root that no project has
// resolved for longer than the policy's MaxAge, keeping the newest
// KeepPerSlug entries for every repository regardless of age.
//
// It is deliberately a package-level function over a root rather than a
// Resolver method: the caller that needs it is machine-wide maintenance
// (forge storage gc), which has no project and no override file, and
// making it a method would force that caller to construct a Resolver
// whose other fields are meaningless to it.
//
// apply=false previews. A missing root is not an error — a machine that
// has never fetched a source has nothing to reclaim.
//
// Evict FAILS CLOSED on anything it cannot read: an entry whose metadata
// is missing, unparseable, or unstattable is retained and reported, never
// removed. Debris from an interrupted fetch is in that set; Resolve
// clears it the next time that exact pin is resolved, which is a cheaper
// place to handle it than a reclaimer that would have to guess.
func Evict(root string, now time.Time, policy EvictPolicy, apply bool, out io.Writer) (Eviction, error) {
	var result Eviction

	dirEntries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("read source cache %s: %w", root, err)
	}

	type candidate struct {
		name    string
		slug    string
		lastUse time.Time
	}
	var usable []candidate

	for _, de := range dirEntries {
		if !de.IsDir() {
			continue
		}
		entry := filepath.Join(root, de.Name())
		if _, ok := readMetadata(entry); !ok {
			result.Kept++
			printf(out, "keep source cache %s (no readable %s — not a complete entry)\n", entry, MetadataFile)
			continue
		}
		info, statErr := os.Stat(filepath.Join(entry, MetadataFile))
		if statErr != nil {
			result.Kept++
			printf(out, "keep source cache %s (cannot read last-use time: %v)\n", entry, statErr)
			continue
		}
		usable = append(usable, candidate{name: de.Name(), slug: entrySlug(de.Name()), lastUse: info.ModTime()})
	}

	bySlug := map[string][]candidate{}
	for _, c := range usable {
		bySlug[c.slug] = append(bySlug[c.slug], c)
	}

	maxAge, keep := policy.maxAge(), policy.keepPerSlug()
	var doomed []candidate
	for _, group := range bySlug {
		sort.Slice(group, func(i, j int) bool { return group[i].lastUse.After(group[j].lastUse) })
		for rank, c := range group {
			if rank < keep {
				result.Kept++
				continue
			}
			if now.Sub(c.lastUse) <= maxAge {
				result.Kept++
				continue
			}
			doomed = append(doomed, c)
		}
	}
	sort.Slice(doomed, func(i, j int) bool { return doomed[i].name < doomed[j].name })

	inUse := policy.InUse
	if inUse == nil && len(doomed) > 0 {
		inUse = openPathProbe()
	}

	for _, c := range doomed {
		entry := filepath.Join(root, c.name)
		if inUse != nil && inUse(entry) {
			result.Kept++
			result.Held = append(result.Held, c.name)
			printf(out, "keep source cache %s (in use by a running process)\n", entry)
			continue
		}
		size := treeSize(entry)
		printf(out, "evict source cache %s (%d bytes, unused %s)\n", entry, size, now.Sub(c.lastUse).Round(time.Hour))
		if apply {
			if err := removeAllWritable(entry); err != nil {
				return result, fmt.Errorf("evict source cache %s: %w", entry, err)
			}
		}
		result.Removed = append(result.Removed, c.name)
		result.Bytes += size
	}
	return result, nil
}

// entrySlug recovers the repository part of a CacheKey ("<slug>-<12 hex>").
// A directory that does not have that shape is its own group, so an
// unrecognized name can never be counted against another repository's
// keep floor.
func entrySlug(name string) string {
	i := strings.LastIndex(name, "-")
	if i <= 0 || len(name)-i-1 != 12 {
		return name
	}
	for _, r := range name[i+1:] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return name
		}
	}
	return name[:i]
}

// touchUsed records that an entry was just resolved. Failure is reported
// to the resolver's log and never to the caller: a cache that cannot
// record its own last-use time still serves the build correctly, and the
// consequence of a missed touch is one re-fetch of a re-fetchable pin.
func touchUsed(entry string, now time.Time) error {
	path := filepath.Join(entry, MetadataFile)
	return os.Chtimes(path, now, now)
}

// treeSize sums the apparent size of every regular file under path.
// Unreadable subtrees contribute nothing rather than failing the sweep —
// this number is reporting, not a decision input.
func treeSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree is reported as zero bytes, not a failure
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// removeAllWritable deletes a tree that may contain read-only files.
// A source entry can hold an installed node_modules and a Go module
// cache, both of which write mode-0444 files and 0555 directories, and
// a plain RemoveAll fails on those with EACCES partway through — leaving
// a half-deleted entry that is worse than the one it replaced.
func removeAllWritable(path string) error {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort widening; RemoveAll reports the real failure
		}
		mode := fs.FileMode(0o600)
		if d.IsDir() {
			mode = 0o700
		}
		if info, err := d.Info(); err == nil {
			mode |= info.Mode().Perm()
		}
		_ = os.Chmod(p, mode)
		return nil
	})
	return os.RemoveAll(path)
}

// openPathProbe builds the in-use predicate from ONE lsof snapshot, taken
// lazily on first use so a preview with no candidates never shells out.
//
// Returns nil when no snapshot is obtainable (lsof absent, Windows, or a
// failing invocation). nil means "cannot tell", and Evict proceeds on the
// recency guard rather than refusing to reclaim anything — see the
// package comment for why that is the safe direction here.
func openPathProbe() func(string) bool {
	if runtime.GOOS == "windows" {
		return nil
	}
	// -Fn emits one field per line, open paths prefixed 'n'. -w silences
	// the permission warnings an unprivileged scan always produces.
	out, err := exec.Command("lsof", "-Fn", "-w").Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	var open []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) > 1 && line[0] == 'n' && line[1] == '/' {
			open = append(open, line[1:])
		}
	}
	return func(entry string) bool {
		prefix := entry + string(os.PathSeparator)
		for _, p := range open {
			if p == entry || strings.HasPrefix(p, prefix) {
				return true
			}
		}
		return false
	}
}

func printf(out io.Writer, format string, args ...any) {
	if out == nil {
		return
	}
	fmt.Fprintf(out, format, args...)
}
