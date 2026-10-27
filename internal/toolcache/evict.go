package toolcache

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/openfiles"
)

// DefaultEvictMaxAge is how long an entry may go unused before it becomes a
// candidate. A rebuild costs a `go install` (a minute or two), so two weeks:
// a pin a developer returns to between sprints survives, and a version every
// project moved off stops costing disk.
const DefaultEvictMaxAge = 14 * 24 * time.Hour

// DefaultEvictKeep is how many of the most recently used entries of one tool
// are retained regardless of age: an idle machine's current pin survives a
// holiday, so the first build after it is not a rebuild.
const DefaultEvictKeep = 1

// legacyGroup prefixes the group of an entry in the module-path layout older
// forges installed codegen plugins in.
const legacyGroup = "legacy/"

// staleStaging is how old an install's staging directory must be before Evict
// considers it for cleanup. Age never proves abandonment: the key lock and
// process inspection must independently permit removal.
const staleStaging = 24 * time.Hour

// EvictPolicy bounds what Evict may remove.
type EvictPolicy struct {
	// MaxAge is the unused age beyond which an entry is a candidate. Zero
	// means DefaultEvictMaxAge.
	MaxAge time.Duration
	// Keep is how many most-recently-used entries of each tool are retained
	// regardless of age. Zero means DefaultEvictKeep; never "keep none".
	Keep int
	// InUse reports whether a live process runs a binary from the entry, holds
	// a file in it open or has its cwd there. nil installs the default
	// open-file scan, taken once and only when there are candidates; when it
	// cannot be taken every candidate is retained.
	InUse func(entry string) bool
	// Ctx bounds the eviction; removals stop at its deadline, retaining the
	// rest. Nil means unbounded.
	Ctx context.Context
	// Unlink authorizes/paces one confined unlink. The callback must call
	// remove, not traverse path itself; only the pinned parent has authority
	// to remove it. Nil removes directly.
	Unlink func(path string, remove func() error) error
	// inspect is the process-snapshot test seam. Production uses openfiles.Take.
	inspect func(context.Context) (openfiles.Snapshot, error)
}

// Eviction reports what Evict did, or would do under apply=false.
type Eviction struct {
	// Removed names the evicted entries, relative to the root.
	Removed []string
	// Bytes is the size of Removed, measured before removal.
	Bytes int64
	// Kept counts the entries retained for any reason.
	Kept int
	// Held names entries retained because a live process was using them.
	Held []string
}

// entry is one installed tool found under the root.
type entry struct {
	parent, name string // the entry is parent/name
	group        string // its tool: entries of one group share the keep floor
	lastUse      time.Time
	locked       bool // the entry has a lock file (current layout)
}

func (e entry) path() string { return filepath.Join(e.parent, e.name) }

// Evict removes tool cache entries under root that nothing has used for
// longer than the policy's MaxAge, keeping the Keep most recently used
// entries of every tool regardless of age. apply=false previews. A missing
// root is not an error: a machine that never installed a tool has nothing to
// reclaim.
//
// Current entries (<root>/<kind>/<key>) record their last use in the
// .forge-tool marker. An entry without one — installed by a forge that
// predates the marker, or in the module-path layout older forges used for
// codegen plugins (<root>/<module>@<version>/) — is dated by its newest file,
// its install time, which is never later than its last use. Anything else
// Evict cannot date is retained. Even dated legacy entries are retained when
// they lack a provable stable version-key lock; age alone is not ownership.
func Evict(root string, now time.Time, policy EvictPolicy, apply bool, out io.Writer) (Eviction, error) {
	var result Eviction
	anchor, err := openCacheDir(root)
	if err != nil {
		printf(out, "keep tool cache %s (cannot confine cache root: %v)\n", root, err)
		return result, nil
	}
	defer anchor.close()
	top, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("read tool cache %s: %w", root, err)
	}
	var entries []entry
	var leftovers []string
	for _, de := range top {
		if !de.IsDir() || strings.HasPrefix(de.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, de.Name())
		if slices.Contains(kinds, de.Name()) {
			found, left, err := kindEntries(dir, de.Name(), now)
			if err != nil {
				return result, err
			}
			entries, leftovers = append(entries, found...), append(leftovers, left...)
			continue
		}
		entries = append(entries, legacyEntries(dir, 0)...)
	}

	var usable []entry
	for _, e := range entries {
		if e.lastUse.IsZero() {
			result.Kept++
			printf(out, "keep tool cache %s (cannot tell when it was last used)\n", e.path())
			continue
		}
		usable = append(usable, e)
	}
	doomed := choose(usable, now, policy, &result)

	inUse := policy.InUse
	if inUse == nil && len(doomed)+len(leftovers) > 0 {
		inspect := policy.inspect
		if inspect == nil {
			inspect = openfiles.Take
		}
		snap, err := inspect(policy.ctx())
		if ctxErr := policy.ctx().Err(); ctxErr != nil {
			result.Kept += len(doomed) + len(leftovers)
			return result, fmt.Errorf("tool cache eviction stopped before removing anything: %w", ctxErr)
		}
		if err != nil {
			result.Kept += len(doomed) + len(leftovers)
			printf(out, "keep %d tool cache entries: cannot determine which are in use (%v)\n", len(doomed), err)
			return result, nil
		}
		inUse = snap.Holds
	}

	policy.InUse = inUse
	if err := finishLeftovers(root, anchor, leftovers, now, policy, apply, out); err != nil {
		return result, err
	}
	for i, e := range doomed {
		if err := policy.ctx().Err(); err != nil {
			result.Kept += len(doomed) - i
			return result, fmt.Errorf("tool cache eviction stopped after %d of %d entries: %w", len(result.Removed), len(doomed), err)
		}
		if inUse(e.path()) {
			result.Kept++
			result.Held = append(result.Held, rel(root, e.path()))
			printf(out, "keep tool cache %s (in use by a running process)\n", e.path())
			continue
		}
		parent, leaf, confErr := anchor.parent(rel(root, e.path()))
		if confErr != nil {
			result.Kept++
			printf(out, "keep tool cache %s (unsafe ancestor: %v)\n", e.path(), confErr)
			continue
		}
		confErr = parent.validateTree(leaf)
		parent.close()
		if confErr != nil {
			result.Kept++
			printf(out, "keep tool cache %s (unsafe tree: %v)\n", e.path(), confErr)
			continue
		}
		size := treeSize(e.path())
		if !apply {
			printf(out, "evict tool cache %s (%d bytes, unused %s)\n", e.path(), size, now.Sub(e.lastUse).Round(time.Hour))
			result.Removed = append(result.Removed, rel(root, e.path()))
			result.Bytes += size
			continue
		}
		pinned, _, err := anchor.parent(rel(root, e.path()))
		if err != nil {
			result.Kept++
			continue
		}
		if err := unchangedRoot(root, anchor); err != nil {
			pinned.close()
			result.Kept++
			continue
		}
		trash, kept, err := moveAsidePinned(e, now, policy, pinned)
		pinned.close()
		if err != nil {
			return result, err
		}
		if kept != "" {
			result.Kept++
			printf(out, "keep tool cache %s (%s)\n", e.path(), kept)
			continue
		}
		printf(out, "evict tool cache %s (%d bytes, unused %s)\n", e.path(), size, now.Sub(e.lastUse).Round(time.Hour))
		result.Removed = append(result.Removed, rel(root, e.path()))
		result.Bytes += size
		if err := policy.removeConfined(root, anchor, trash); err != nil {
			return result, fmt.Errorf("evict tool cache %s: %w", e.path(), err) // out of the cache; finished next pass
		}
	}
	return result, nil
}

// choose groups entries by tool and returns those beyond the keep floor that
// are older than MaxAge, counting the rest as kept.
func choose(entries []entry, now time.Time, policy EvictPolicy, result *Eviction) []entry {
	byGroup := map[string][]entry{}
	for _, e := range entries {
		byGroup[e.group] = append(byGroup[e.group], e)
	}
	var doomed []entry
	for name, group := range byGroup {
		sort.Slice(group, func(i, j int) bool { return group[i].lastUse.After(group[j].lastUse) })
		// The legacy layout has no keep floor: this forge never resolves it,
		// so its newest entry is not anyone's current pin, and an older forge
		// still using it reinstalls on a miss.
		floor := policy.keep()
		if strings.HasPrefix(name, legacyGroup) {
			floor = 0
		}
		for rank, e := range group {
			if rank < floor || now.Sub(e.lastUse) <= policy.maxAge() {
				result.Kept++
				continue
			}
			doomed = append(doomed, e)
		}
	}
	sort.Slice(doomed, func(i, j int) bool { return doomed[i].path() < doomed[j].path() })
	return doomed
}

// moveAside renames a doomed entry out of the cache under its key's
// exclusive lock, after re-reading its last use: a resolver that touched it
// since the scan keeps it. It returns the trash path, or why it was kept.
func moveAside(e entry, now time.Time, policy EvictPolicy) (string, string, error) {
	parent, err := openCacheDir(e.parent)
	if err != nil {
		return "", "unconfined parent", nil
	}
	defer parent.close()
	return moveAsidePinned(e, now, policy, parent)
}

func moveAsidePinned(e entry, now time.Time, policy EvictPolicy, parent *cacheDir) (trash, kept string, err error) {
	trashName := fmt.Sprintf("%s%s-%d", evictingPrefix, e.name, now.UnixNano())
	trash = filepath.Join(e.parent, trashName)
	move := func() error {
		if last := lastUse(e.path()); last.IsZero() || now.Sub(last) <= policy.maxAge() {
			kept = "used since this pass began"
			return nil
		}
		if policy.InUse != nil && policy.InUse(e.path()) {
			kept = "in use by a running process"
			return nil
		}
		if err := parent.validateTree(e.name); err != nil {
			kept = "unsafe or changed tree"
			return nil
		}
		return parent.root.Rename(e.name, trashName)
	}
	if !e.locked {
		return "", "no provable version-key lock", nil
	}
	locks, err := parent.child(lockDir, true)
	if err != nil {
		return "", "unsafe lock directory", nil
	}
	defer locks.close()
	f, err := locks.openRegular(e.name + ".lock")
	if err != nil {
		return "", "unsafe or missing key lock", nil
	}
	defer func() { _ = f.Close() }()
	ok, err := tryLock(f, true)
	if err != nil || !ok {
		return "", "being resolved or installed right now", nil
	}
	defer unlock(f)
	err = move()
	return trash, kept, err
}

// kindEntries lists the entries of one kind directory, and the leftovers of
// interrupted evictions and abandoned installs in it.
func kindEntries(dir, kind string, now time.Time) (entries []entry, leftovers []string, err error) {
	children, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read tool cache %s: %w", dir, err)
	}
	for _, c := range children {
		name := c.Name()
		path := filepath.Join(dir, name)
		switch {
		case !c.IsDir() || name == lockDir:
		case strings.HasPrefix(name, evictingPrefix), strings.HasPrefix(name, stagingPrefix):
			if _, ok := leftoverKey(name); ok {
				if info, err := c.Info(); err == nil && (strings.HasPrefix(name, evictingPrefix) || now.Sub(info.ModTime()) > staleStaging) {
					leftovers = append(leftovers, path)
				}
			}
		case strings.HasPrefix(name, "."):
			// Unknown/legacy dot directories cannot be attributed to an install key.

		default:
			// Never infer that a missing lock means no owner. An unrecognized
			// or pre-lock entry is retained until ownership can be established.
			entries = append(entries, entry{parent: dir, name: name, group: kind + "/" + toolName(name, kind), lastUse: lastUse(path), locked: true})
		}
	}
	return entries, leftovers, nil
}

// legacyEntries finds the module-path layout older forges installed codegen
// plugins in (<root>/google.golang.org/protobuf@v1.36.12/), at most a few
// levels deep: a directory named <something>@<version> is one entry.
func legacyEntries(dir string, depth int) []entry {
	if depth > 5 {
		return nil
	}
	children, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []entry
	for _, c := range children {
		if !c.IsDir() || strings.HasPrefix(c.Name(), ".") {
			continue
		}
		if strings.Contains(c.Name(), "@") {
			path := filepath.Join(dir, c.Name())
			out = append(out, entry{parent: dir, name: c.Name(), group: legacyGroup + toolName(c.Name(), ""), lastUse: lastUse(path), locked: true})
			continue
		}
		out = append(out, legacyEntries(filepath.Join(dir, c.Name()), depth+1)...)
	}
	return out
}

// toolName is the tool an entry key belongs to: "buf" for "buf@v1.57.0+go1.27.3",
// and the kind itself for a key that names only a version (golangci-lint's
// "v2.14.0-go1.27.2").
func toolName(key, kind string) string {
	if i := strings.Index(key, "@"); i > 0 {
		return key[:i]
	}
	return kind
}

// lastUse is the entry's .forge-tool mtime, else the newest of its files' (an
// entry that predates the marker), else zero.
func lastUse(path string) time.Time {
	if info, err := os.Stat(filepath.Join(path, UsedMarker)); err == nil {
		return info.ModTime()
	}
	children, err := os.ReadDir(path)
	if err != nil {
		return time.Time{}
	}
	var newest time.Time
	for _, c := range children {
		if !c.Type().IsRegular() {
			continue
		}
		if info, err := c.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// leftoverKey recovers the exact version key. Stage suffixes are MkdirTemp
// decimal random IDs; eviction suffixes are UnixNano timestamps. Ambiguous or
// legacy names have no ownership proof and are retained.
func leftoverKey(name string) (string, bool) {
	var body string
	switch {
	case strings.HasPrefix(name, stagingPrefix):
		body = strings.TrimPrefix(name, stagingPrefix)
	case strings.HasPrefix(name, evictingPrefix):
		body = strings.TrimPrefix(name, evictingPrefix)
	default:
		return "", false
	}
	i := strings.LastIndexByte(body, '-')
	if i <= 0 || i == len(body)-1 {
		return "", false
	}
	for _, r := range body[i+1:] {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	key := body[:i]
	return key, validKey(key) == nil
}

// finishLeftovers uses the SAME version lock and process checks as installs.
// Directory mtime is only an eligibility filter, never proof of abandonment.
func finishLeftovers(root string, anchor *cacheDir, leftovers []string, now time.Time, policy EvictPolicy, apply bool, out io.Writer) error {
	sort.Strings(leftovers)
	for _, path := range leftovers {
		if err := policy.ctx().Err(); err != nil {
			return err
		}
		if err := unchangedRoot(root, anchor); err != nil {
			printf(out, "keep tool cache leftover %s (%v)\n", path, err)
			continue
		}
		key, ok := leftoverKey(filepath.Base(path))
		if !ok {
			continue
		}
		parent, leaf, err := anchor.parent(rel(root, path))
		if err != nil {
			printf(out, "keep tool cache leftover %s (%v)\n", path, err)
			continue
		}
		if err = parent.validateTree(leaf); err != nil {
			parent.close()
			printf(out, "keep tool cache leftover %s (%v)\n", path, err)
			continue
		}
		// Opening a symlinked .locks directory or lock file is never allowed.
		locks, err := parent.child(lockDir, true)
		if err != nil {
			parent.close()
			printf(out, "keep tool cache leftover %s (no safe key lock)\n", path)
			continue
		}
		f, err := locks.openRegular(key + ".lock")
		if err != nil {
			locks.close()
			parent.close()
			continue
		}
		locked, err := tryLock(f, true)
		if err != nil || !locked {
			_ = f.Close()
			locks.close()
			parent.close()
			printf(out, "keep tool cache leftover %s (key locked or uncertain)\n", path)
			continue
		}
		removeErr := func() error {
			defer unlock(f)
			if policy.InUse == nil || policy.InUse(path) || policy.InUse(filepath.Join(filepath.Dir(path), key)) {
				printf(out, "keep tool cache leftover %s (in use or process inspection unavailable)\n", path)
				return nil
			}
			current, err := parent.root.Lstat(leaf)
			if err != nil || !current.IsDir() {
				return nil
			}
			if strings.HasPrefix(leaf, stagingPrefix) && now.Sub(current.ModTime()) <= staleStaging {
				return nil
			}
			printf(out, "remove tool cache leftover %s\n", path)
			if apply {
				return policy.removeConfined(root, anchor, path)
			}
			return nil
		}()
		_ = f.Close()
		locks.close()
		parent.close()
		if removeErr != nil {
			return removeErr
		}
	}
	return nil
}

func (p EvictPolicy) ctx() context.Context {
	if p.Ctx != nil {
		return p.Ctx
	}
	return context.Background()
}

func (p EvictPolicy) maxAge() time.Duration {
	if p.MaxAge <= 0 {
		return DefaultEvictMaxAge
	}
	return p.MaxAge
}

func (p EvictPolicy) keep() int {
	if p.Keep <= 0 {
		return DefaultEvictKeep
	}
	return p.Keep
}

// removeConfined verifies root identity again and traverses only descriptor-
// relative no-follow children. Never delegate recursive path traversal.
func unchangedRoot(root string, anchor *cacheDir) error {
	check, err := openCacheDir(root)
	if err != nil {
		return err
	}
	defer check.close()
	before, err := anchor.file.Stat()
	if err != nil {
		return err
	}
	after, err := check.file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return fmt.Errorf("retain cache: root inode changed")
	}
	return nil
}

func (p EvictPolicy) removeConfined(root string, anchor *cacheDir, path string) error {
	if p.InUse != nil && p.InUse(path) {
		return fmt.Errorf("retain %s: in use", path)
	}
	check, err := openCacheDir(root)
	if err != nil {
		return err
	}
	defer check.close()
	before, err := anchor.file.Stat()
	if err != nil {
		return err
	}
	after, err := check.file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return fmt.Errorf("retain %s: cache root inode changed", path)
	}
	parent, leaf, err := anchor.parent(rel(root, path))
	if err != nil {
		return err
	}
	defer parent.close()
	if err := parent.validateTree(leaf); err != nil {
		return err
	}
	return parent.removeTree(leaf, path, p.Unlink)
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(r)
	}
	return path
}

// treeSize sums the size of every regular file under path; reporting only.
func treeSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree counts as zero bytes
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func printf(out io.Writer, format string, args ...any) {
	if out != nil {
		fmt.Fprintf(out, format, args...)
	}
}
