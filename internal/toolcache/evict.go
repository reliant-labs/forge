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
// treats it as abandoned. A build that takes a day is not running.
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
	// RemoveTree deletes one evicted entry's tree (maintenance passes its
	// paced deleter). Nil removes it directly.
	RemoveTree func(path string) error
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
// Evict cannot date is retained. The module-path layout gets no keep floor:
// this forge never resolves it, so age alone decides.
func Evict(root string, now time.Time, policy EvictPolicy, apply bool, out io.Writer) (Eviction, error) {
	var result Eviction
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

	if err := finishLeftovers(leftovers, policy, apply, out); err != nil {
		return result, err
	}

	inUse := policy.InUse
	if inUse == nil && len(doomed) > 0 {
		snap, err := openfiles.Take(policy.ctx())
		if ctxErr := policy.ctx().Err(); ctxErr != nil {
			result.Kept += len(doomed)
			return result, fmt.Errorf("tool cache eviction stopped before removing anything: %w", ctxErr)
		}
		if err != nil {
			result.Kept += len(doomed)
			printf(out, "keep %d tool cache entries: cannot determine which are in use (%v)\n", len(doomed), err)
			return result, nil
		}
		inUse = snap.Holds
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
		size := treeSize(e.path())
		if !apply {
			printf(out, "evict tool cache %s (%d bytes, unused %s)\n", e.path(), size, now.Sub(e.lastUse).Round(time.Hour))
			result.Removed = append(result.Removed, rel(root, e.path()))
			result.Bytes += size
			continue
		}
		trash, kept, err := moveAside(e, now, policy)
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
		if err := policy.removeTree(trash); err != nil {
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
func moveAside(e entry, now time.Time, policy EvictPolicy) (trash, kept string, err error) {
	trash = filepath.Join(e.parent, fmt.Sprintf("%s%s-%d", evictingPrefix, e.name, now.UnixNano()))
	move := func() error {
		if last := lastUse(e.path()); last.IsZero() || now.Sub(last) <= policy.maxAge() {
			kept = "used since this pass began"
			return nil
		}
		if err := os.Rename(e.path(), trash); err != nil {
			return fmt.Errorf("evict tool cache %s: %w", e.path(), err)
		}
		return nil
	}
	if !e.locked {
		// The pre-marker layouts have no lock file, and nothing current
		// resolves them, so there is no resolver to race.
		return trash, kept, move()
	}
	f, err := os.OpenFile(lockPath(e.parent, e.name), os.O_RDWR, 0)
	if err != nil {
		return "", "", fmt.Errorf("evict tool cache %s: %w", e.path(), err)
	}
	defer func() { _ = f.Close() }()
	ok, err := tryLock(f, true)
	if err != nil {
		return "", "", fmt.Errorf("evict tool cache %s: %w", e.path(), err)
	}
	if !ok {
		return "", "being resolved or installed right now", nil
	}
	defer unlock(f)
	return trash, kept, move()
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
		case strings.HasPrefix(name, evictingPrefix):
			leftovers = append(leftovers, path)
		case strings.HasPrefix(name, "."):
			// An install's staging dir (.build-*, and .install-* from older
			// forges): abandoned once nothing has written it for a day.
			if info, err := c.Info(); err == nil && now.Sub(info.ModTime()) > staleStaging {
				leftovers = append(leftovers, path)
			}
		default:
			_, statErr := os.Stat(lockPath(dir, name))
			entries = append(entries, entry{parent: dir, name: name, group: kind + "/" + toolName(name, kind), lastUse: lastUse(path), locked: statErr == nil})
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
			out = append(out, entry{parent: dir, name: c.Name(), group: legacyGroup + toolName(c.Name(), ""), lastUse: lastUse(path)})
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

// finishLeftovers removes what interrupted evictions renamed away and what
// abandoned installs left. Their names are no entry key, so nothing resolves
// them.
func finishLeftovers(leftovers []string, policy EvictPolicy, apply bool, out io.Writer) error {
	sort.Strings(leftovers)
	for _, path := range leftovers {
		if err := policy.ctx().Err(); err != nil {
			return fmt.Errorf("tool cache eviction stopped before clearing leftovers: %w", err)
		}
		printf(out, "remove tool cache leftover %s\n", path)
		if apply {
			if err := policy.removeTree(path); err != nil {
				return fmt.Errorf("remove tool cache leftover %s: %w", path, err)
			}
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

func (p EvictPolicy) removeTree(path string) error {
	if p.RemoveTree != nil {
		return p.RemoveTree(path)
	}
	return removeAllWritable(path)
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

// removeAllWritable deletes a tree that may hold read-only files or
// directories (a go-installed binary is 0555 on some systems).
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

func printf(out io.Writer, format string, args ...any) {
	if out != nil {
		fmt.Fprintf(out, format, args...)
	}
}
