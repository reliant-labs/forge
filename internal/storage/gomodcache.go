package storage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/mod/module"
)

// The shared module cache (GOMODCACHE) layer.
//
// Go has no trim for the module cache: `go clean -modcache` is all or nothing
// and fails every build running beside it. On a shared dev machine the cache
// only grows — every worktree of every project adds the versions its go.sum
// names, and 46 GB of 2.1 million files accumulated, 31 GB three days earlier.
//
// # What is evicted, and in what order
//
// The unit is one module version: its extracted tree (<escaped path>@<ver>/)
// and its download-cache zip + ziphash (cache/download/<path>/@v/<ver>.zip).
// The .info and .mod files are tiny and keep `go list -m`/graph loading
// working offline, so they stay. Eviction has two tiers, cheapest to recover
// first:
//
//  1. the extracted tree: cmd/go re-extracts it from the zip on the next use,
//     offline, in seconds;
//  2. the zip and ziphash (and the tree, which cmd/go would otherwise discard
//     itself once the ziphash is gone): the next use downloads it again.
//
// Candidates are taken in this order until the cache is down to the low-water
// mark of its budget (planModCacheEviction):
//
//	unreferenced tiers 1, 2  -> idle-checkout-referenced tiers 1, 2
//
// least recently used first within each. Nothing else is ever removed:
// toolchains (golang.org/toolchain@*, one of which is the running `go`), the
// vcs cache and the sumdb are not units.
//
// # "Least recently used" — the signal
//
// cmd/go records no use of a module version, so there is nothing to read.
// The candidates, as measured on the machine this was written for (2,085
// versions):
//
//   - atime of the files or the tree. Rejected: 98% of the extracted trees'
//     go.mod files were read in the last 24 hours, because every scanner on
//     the machine (indexers, the open-files sweeps, du) touches them. On a
//     relatime mount atime also moves at most once a day, and on noatime not
//     at all.
//   - a use ledger. Rejected: nothing would feed it. cmd/go does not call out,
//     and wrapping `go` is a promise every tool on the machine must keep.
//   - the newest mtime among the tree, the zip, .info and .mod: when the
//     version was last fetched or re-queried. CHOSEN, but as the weak half of
//     the rule. A version in active use is not recognised by this timestamp;
//     it is recognised by being referenced (below). The timestamp only orders
//     what nothing references, and keeps whatever was just fetched.
//
// # What protects a version
//
// A version is pinned (never evicted) when any of these holds:
//
//   - it was fetched or touched in the last 24 hours (modCacheFloor): a fresh
//     `go get` or a build that downloaded it mid-run. The live-build lease of
//     liveBuildBound also applies, so a build older than the floor still holds
//     back the cutoff to its start;
//   - a live process names it: `go run x@v`, `go install x@v` carry the
//     version in their arguments, which the pass reads (processEnvText);
//   - a live process has a file open under its tree (the shared open-files
//     snapshot);
//   - its go.sum entry is in a checkout that is ACTIVE: one a process holds
//     (a cwd or open file under it) or whose go.sum/go.mod changed in the last
//     24 hours. This is "never evict a version referenced by a checkout that
//     has a running process", and it covers every build started from one: a
//     build needs exactly the zip-bearing entries of its go.sum.
//
// A version referenced only by an idle checkout is evictable, but only after
// everything unreferenced is gone and the cache is still over budget: an idle
// checkout is a cost to re-download, not a break. Checkouts are the worktrees
// git lists for every registered project and repo (Policy.Projects/Repos);
// their go.sum files come from `git ls-files`, so a node_modules is never walked.
//
// # What it cannot see
//
// A build in a checkout nobody registered, started AFTER the open-files
// snapshot and naming no pinned version in its arguments, can lose a version
// it needs between the snapshot and its first read. The 24-hour floor, the
// reference rule and the snapshot together make that window the length of a
// pass over a version no registered checkout names, fetched a day ago or
// earlier; the failure is a download, never a corrupt tree (the tree is
// renamed out of the cache before it is deleted, so no reader sees half of it).
//
// The pass refuses to run on evidence it cannot read: no checkout lists any
// go.sum (the references are unknown), the process list or the open-files
// snapshot cannot be taken.

// modCacheFloor is the youngest a version's last-use time may be for it to be
// evictable, under every rule and any budget pressure. It is not a knob.
const modCacheFloor = 24 * time.Hour

// modCacheAsideDir holds trees renamed out of the cache and not yet deleted,
// under the cache's own `cache/` directory. Go's tools ignore it.
const modCacheAsideDir = "forge-evicting"

type modClass int

const (
	modFree    modClass = iota // referenced by no checkout
	modIdleRef                 // referenced by idle checkouts only
	modPinned                  // never evicted
)

// modUnit is one module version in the cache.
type modUnit struct {
	key  string // "<escaped path>@<version>"
	dir  string // extracted tree; "" when absent
	zip  string
	hash string
	lock string

	dirBytes, zipBytes int64
	dirFiles           int
	lastUse            time.Time
	class              modClass
	why                string // what pinned it, for the report
}

func (u *modUnit) bytes() int64 { return u.dirBytes + u.zipBytes }

// modAction is one eviction: the extracted tree only, or the tree and the zip.
type modAction struct {
	unit *modUnit
	full bool
}

// planModEviction decides what to evict to bring the cache from its current
// size to target, or states that it cannot. It is pure: the caller measured
// and classified.
//
// Phases, in order, each least recently used first:
//
//	free tier 1, free tier 2, idle-referenced tier 1, idle-referenced tier 2.
func planModEviction(units []*modUnit, target int64) (actions []modAction, total, pinned, left int64) {
	for _, u := range units {
		total += u.bytes()
		if u.class == modPinned {
			pinned += u.bytes()
		}
	}
	left = total
	treeGone := map[*modUnit]bool{}
	for _, phase := range []struct {
		class modClass
		full  bool
	}{{modFree, false}, {modFree, true}, {modIdleRef, false}, {modIdleRef, true}} {
		var in []*modUnit
		for _, u := range units {
			if u.class == phase.class {
				in = append(in, u)
			}
		}
		sort.SliceStable(in, func(i, j int) bool { return in[i].lastUse.Before(in[j].lastUse) })
		for _, u := range in {
			if left <= target {
				return actions, total, pinned, left
			}
			var freed int64
			if !treeGone[u] {
				freed += u.dirBytes
			}
			if phase.full {
				freed += u.zipBytes
			}
			if freed == 0 {
				continue
			}
			treeGone[u] = true
			actions = append(actions, modAction{unit: u, full: phase.full})
			left -= freed
		}
	}
	return actions, total, pinned, left
}

// GoModCache bounds the shared module cache to go_mod_cache_gib. Dry-run unless apply.
func (r Runner) GoModCache(apply bool) error {
	_, root, err := r.goEnv(r.hostCtx())
	if err != nil {
		r.print("skip go module cache (%v)\n", err)
		if testingWithoutRoot(err) {
			return nil
		}
		return fmt.Errorf("locate the Go module cache: %w", err)
	}
	if root == "" {
		return nil // no module cache configured (a test that did not name one)
	}
	if !isGoModCacheDir(root) {
		return nil // absent, or not a module cache
	}
	ctx := r.hostCtx()
	del := r.deletes()
	budget := int64(r.modCacheBudget(root))
	target := int64(float64(budget) * goCacheLowWater)
	now := time.Now()

	// Trees renamed out of the cache by an earlier pass are decided
	// evictions: finish them before measuring, or they count against the budget.
	if err := r.finishModEvictions(ctx, del, root, apply); err != nil {
		return err
	}

	units, err := scanModCache(ctx, root, del)
	if err != nil {
		return r.modScanStopped(err)
	}
	var cacheBytes int64
	for _, u := range units {
		cacheBytes += u.bytes()
	}
	if cacheBytes <= budget {
		r.print("go module cache %s: %d versions, %.1f GiB, within its %.1f GiB budget\n", root, len(units), gib(cacheBytes), gib(budget))
		r.recordModTrim(GoCacheTrim{Root: root, Applied: apply, EstimatedBytes: uint64(cacheBytes), BudgetBytes: uint64(budget), Finished: true})
		return nil
	}

	// Over budget: only now is the (slower) evidence gathered.
	cutoff := now.Add(-modCacheFloor)
	var liveBuild time.Duration
	if bound, oldest, ok := r.liveBuildBound(ctx, now); ok && bound.Before(cutoff) {
		cutoff, liveBuild = bound, oldest
	}
	procs, err := r.processEnvText(ctx)
	if err != nil {
		return r.modEvidenceMissing("which versions live processes name", err)
	}
	open, err := r.goCacheOpenPaths(ctx)
	if err != nil {
		return r.modEvidenceMissing("which trees are open", err)
	}
	refs, err := r.collectModRefs(ctx, open, now)
	if err != nil {
		return r.modEvidenceMissing("which versions checkouts reference", err)
	}
	var byClass [3]int64
	for _, u := range units {
		classifyModUnit(u, refs, procs, open, cutoff)
		byClass[u.class] += u.bytes()
	}

	actions, total, pinned, left := planModEviction(units, target)
	r.print("go module cache %s: %d versions, %.1f GiB (budget %.1f GiB, trimmed to %.1f GiB when over); pinned %.1f GiB (%s), referenced only by idle checkouts %.1f GiB, unreferenced %.1f GiB; %d checkouts, %d active\n",
		root, len(units), gib(total), gib(budget), gib(target), gib(pinned), refs.summary(), gib(byClass[modIdleRef]), gib(byClass[modFree]), refs.checkouts, refs.active)

	var files int
	planned := map[*modUnit]bool{}
	for _, a := range actions {
		if !planned[a.unit] {
			files += a.unit.dirFiles
		}
		planned[a.unit] = true
	}
	// A cache over its budget may unlink what it takes to get back under it,
	// beyond the per-pass cap (deletes.go): 1.3M files is 26 passes of the cap.
	del = del.allowOver(files + files/10 + deleteBatch)

	done, freed, stop := r.runModEvictions(ctx, del, root, actions, apply)
	versions := map[*modUnit]bool{}
	for _, a := range actions[:done] {
		versions[a.unit] = true
	}
	for i, a := range actions {
		if i >= 5 {
			break
		}
		r.print("  %s %s%s (%.1f MiB, last used %s)\n", verb(apply), a.unit.key, map[bool]string{true: " and its zip", false: " tree"}[a.full], float64(a.unit.bytes())/(1<<20), a.unit.lastUse.Format("2006-01-02 15:04"))
	}
	status := "finished"
	if stop != nil {
		status = fmt.Sprintf("stopped after %d/%d steps: %v", done, len(actions), stop)
	}
	r.print("go module cache: %s %d versions (%d steps), %.1f GiB (%s)\n", map[bool]string{true: "removed", false: "would remove"}[apply], len(versions), done, gib(freed), status)
	r.recordModTrim(GoCacheTrim{Root: root, Applied: apply, EstimatedBytes: uint64(total), BudgetBytes: uint64(budget), YoungBytes: uint64(pinned),
		Cutoff: cutoff, Removed: done, FreedBytes: uint64(max(freed, 0)), ShardsCovered: done, ShardsTotal: len(actions), Finished: stop == nil, OldestLiveBuild: liveBuild})

	var failures []error
	if stop != nil && !(errors.Is(stop, context.DeadlineExceeded) || errors.Is(stop, context.Canceled)) {
		failures = append(failures, stop)
	} else if stop != nil {
		failures = append(failures, &CutOffError{Err: stop})
	}
	if stop == nil && left > budget {
		unreachable := &BudgetUnreachableError{Cache: "go module cache", Estimated: uint64(total), Budget: uint64(budget), Young: uint64(pinned), Floor: modCacheFloor,
			Reason: "the rest is pinned: used or fetched in the last day, named by a running process, open, or referenced by a checkout a process holds (or changed in the last day). Raise go_mod_cache_gib, or retire the worktrees (forge storage worktrees)"}
		r.print("WARNING %v\n", unreachable)
		failures = append(failures, unreachable)
	}
	return errors.Join(failures...)
}

// modEvidenceMissing is the layer's result when the evidence a deletion needs
// could not be gathered: nothing is removed. A deadline is a cut-off, so the
// pass records that it resumes; anything else is reported and also retained.
func (r Runner) modEvidenceMissing(what string, err error) error {
	r.print("go module cache: nothing evicted, cannot determine %s (%v)\n", what, err)
	if onlyContextErrors(err) {
		return &CutOffError{Err: err}
	}
	return nil
}

func gib(b int64) float64 { return float64(b) / float64(GiB) }

// testingWithoutRoot reports the error goEnv returns for a test that named no
// root, which is a skip and not a failure.
func testingWithoutRoot(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is unset under test")
}

func (r Runner) modCacheBudget(root string) uint64 {
	return r.budgetFor(r.Policy.GoModCacheGiB, root)
}

func (r Runner) recordModTrim(t GoCacheTrim) {
	if r.OnGoCacheTrim == nil {
		return
	}
	t.Cache = "go module cache"
	r.OnGoCacheTrim(t)
}

// modScanStopped turns a stopped scan into the layer's result: a deadline is a
// cut-off, filesystem distress a back-off, and nothing was removed either way.
func (r Runner) modScanStopped(err error) error {
	r.print("go module cache: measuring stopped before removing anything: %v\n", err)
	if onlyContextErrors(err) {
		return &CutOffError{Err: err}
	}
	return err
}

// runModEvictions executes actions in order through the governor, stopping at
// the first reason the pass must end. It returns the versions handled, the
// bytes they held, and the stop, if any.
func (r Runner) runModEvictions(ctx context.Context, del deleter, root string, actions []modAction, apply bool) (done int, freed int64, stop error) {
	seq := 0
	treeCounted := map[*modUnit]bool{}
	for _, a := range actions {
		if err := ctx.Err(); err != nil {
			return done, freed, err
		}
		u := a.unit
		var bytes int64
		if apply {
			var err error
			if bytes, err = r.evictModUnit(ctx, del, root, u, a.full, &seq); err != nil {
				return done, freed, err
			}
		} else {
			if !treeCounted[u] {
				bytes = u.dirBytes
				treeCounted[u] = true
			}
			if a.full {
				bytes += u.zipBytes
			}
		}
		done++
		freed += bytes
	}
	return done, freed, nil
}

// evictModUnit removes one version's tree, and with full its zip. The version's
// own lock (the one cmd/go takes to download or extract it) is taken first; a
// version being fetched right now is skipped. The tree is renamed out of the
// cache before it is deleted, so a reader never sees half of it.
func (r Runner) evictModUnit(ctx context.Context, del deleter, root string, u *modUnit, full bool, seq *int) (int64, error) {
	unlock, err := tryLockFile(u.lock)
	if err != nil {
		return 0, nil // being downloaded or extracted: retained, the next pass reconsiders it
	}
	defer unlock()
	var freed int64
	if u.dir != "" {
		if _, err := os.Lstat(u.dir); err == nil {
			*seq++
			aside := filepath.Join(root, "cache", modCacheAsideDir, fmt.Sprintf("%d-%d", time.Now().UnixNano(), *seq))
			if err := os.MkdirAll(filepath.Dir(aside), 0o755); err != nil {
				return freed, err
			}
			// cmd/go writes the tree 0555, and moving a directory to another
			// parent rewrites its ".." entry, which needs write permission on it.
			_ = os.Chmod(u.dir, 0o755)
			if err := os.Rename(u.dir, aside); err != nil {
				return freed, del.observe(u.dir, err)
			}
			freed += u.dirBytes
			if err := del.removeTree(ctx, aside); err != nil {
				return freed, err // the leftover is finished by the next pass
			}
		}
	}
	if full {
		// ziphash first: a zip without one is rebuilt from the zip, a ziphash
		// without a zip is nothing cmd/go trusts.
		for _, p := range []string{u.hash, u.zip} {
			if err := del.unlink(ctx, p); err != nil {
				return freed, err
			}
		}
		freed += u.zipBytes
	}
	return freed, nil
}

// finishModEvictions deletes trees an earlier pass renamed out of the cache.
func (r Runner) finishModEvictions(ctx context.Context, del deleter, root string, apply bool) error {
	dir := filepath.Join(root, "cache", modCacheAsideDir)
	left, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	del = del.allowOver(1 << 30)
	for _, e := range left {
		p := filepath.Join(dir, e.Name())
		r.print("finish interrupted go module cache eviction %s\n", p)
		if !apply {
			continue
		}
		if err := del.removeTree(ctx, p); err != nil {
			if onlyContextErrors(err) {
				return &CutOffError{Err: err}
			}
			return err
		}
	}
	return nil
}

// scanModCache lists the cache's module versions with their sizes. Sizes come
// from walking each extracted tree; four workers, stopping at the deadline.
func scanModCache(ctx context.Context, root string, del deleter) ([]*modUnit, error) {
	download := filepath.Join(root, "cache", "download")
	var units []*modUnit
	var walk func(dir string) error
	walk = func(dir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return del.observe(dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if e.Name() == "@v" {
				rel, err := filepath.Rel(download, dir)
				if err != nil {
					continue
				}
				units = append(units, modUnitsIn(root, filepath.ToSlash(rel), filepath.Join(dir, "@v"))...)
				continue
			}
			if dir == download && e.Name() == "sumdb" {
				continue
			}
			if err := walk(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(download); err != nil {
		return nil, err
	}

	var wg sync.WaitGroup
	var next atomic.Int64
	var stopErr error
	var mu sync.Mutex
	for w := 0; w < goCacheWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(units) || ctx.Err() != nil {
					return
				}
				u := units[i]
				if u.dir == "" {
					continue
				}
				bytes, files, err := treeStats(ctx, u.dir)
				if err != nil {
					mu.Lock()
					if stopErr == nil {
						stopErr = del.observe(u.dir, err)
					}
					mu.Unlock()
					return
				}
				u.dirBytes, u.dirFiles = bytes, files
			}
		}()
	}
	wg.Wait()
	if stopErr != nil {
		return nil, stopErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return units, nil
}

// modUnitsIn lists the versions that have a zip in one module's @v directory.
// rel is the module's escaped path (slash separated).
func modUnitsIn(root, rel, vdir string) []*modUnit {
	files, err := os.ReadDir(vdir)
	if err != nil {
		return nil
	}
	var out []*modUnit
	for _, f := range files {
		ver, ok := strings.CutSuffix(f.Name(), ".zip")
		if !ok || f.IsDir() {
			continue
		}
		base := filepath.Join(vdir, ver)
		zi, err := os.Stat(base + ".zip")
		if err != nil {
			continue
		}
		u := &modUnit{key: rel + "@" + ver, zip: base + ".zip", hash: base + ".ziphash", lock: base + ".lock", zipBytes: zi.Size(), lastUse: zi.ModTime()}
		if hi, err := os.Stat(u.hash); err == nil {
			u.zipBytes += hi.Size()
			u.lastUse = maxTime(u.lastUse, hi.ModTime())
		}
		for _, ext := range []string{".info", ".mod"} {
			if fi, err := os.Stat(base + ext); err == nil {
				u.lastUse = maxTime(u.lastUse, fi.ModTime())
			}
		}
		dir := filepath.Join(root, filepath.FromSlash(rel)+"@"+ver)
		if di, err := os.Lstat(dir); err == nil && di.IsDir() {
			u.dir = dir
			u.lastUse = maxTime(u.lastUse, di.ModTime())
		}
		out = append(out, u)
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// treeStats is the bytes and the number of entries (files and directories, one
// governed unlink each) of a tree, not following symlinks.
func treeStats(ctx context.Context, dir string) (bytes int64, files int, err error) {
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, werr error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if werr != nil {
			if fsDistress(werr) {
				return werr
			}
			return nil // removed since the listing, or unreadable: counted as nothing
		}
		files++ // directories count: removing a tree unlinks each one through the governor
		if d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			bytes += info.Size()
		}
		return nil
	})
	return bytes, files, err
}

// modRefs is what the checkouts of the machine reference in the cache.
type modRefs struct {
	// activeRefs and idleRefs hold "<escaped path>@<version>" for each go.sum
	// entry that carries a zip hash, split by whether the checkout is active.
	activeRefs, idleRefs map[string]bool
	checkouts, active    int
}

func (m *modRefs) summary() string {
	return fmt.Sprintf("%d versions referenced by active checkouts", len(m.activeRefs))
}

// collectModRefs reads the go.sum of every checkout git lists for the registered
// projects and repos. A checkout is active when a process holds it or its
// go.sum/go.mod changed within modCacheFloor. It fails when no go.sum was read:
// with no references known, "unreferenced" would mean everything.
func (r Runner) collectModRefs(ctx context.Context, open interface{ Holds(string) bool }, now time.Time) (*modRefs, error) {
	refs := &modRefs{activeRefs: map[string]bool{}, idleRefs: map[string]bool{}}
	seen := map[string]bool{}
	var checkouts []string
	for _, project := range append(append([]string(nil), r.Policy.Projects...), r.Policy.Repos...) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out, err := r.command(ctx, "git", "--no-optional-locks", "-C", project, "worktree", "list", "--porcelain")
		if err != nil {
			continue
		}
		for _, e := range parseWorktrees(string(out)) {
			if p := canonicalPath(e.path); !seen[p] && !e.bare {
				seen[p] = true
				checkouts = append(checkouts, p)
			}
		}
	}
	read := 0
	for _, co := range checkouts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out, err := r.command(ctx, "git", "--no-optional-locks", "-C", co, "ls-files", "-z", "--", "go.sum", "go.work.sum", "*/go.sum", "*/go.work.sum")
		if err != nil {
			continue
		}
		var sums []string
		for _, name := range strings.Split(string(out), "\x00") {
			if name != "" {
				sums = append(sums, filepath.Join(co, filepath.FromSlash(name)))
			}
		}
		if len(sums) == 0 {
			continue
		}
		active := open.Holds(co)
		if !active {
			for _, f := range append(sums, filepath.Join(co, "go.mod")) {
				if fi, err := os.Stat(f); err == nil && now.Sub(fi.ModTime()) < modCacheFloor {
					active = true
					break
				}
			}
		}
		refs.checkouts++
		if active {
			refs.active++
		}
		for _, f := range sums {
			keys, err := goSumZipKeys(f)
			if err != nil {
				continue
			}
			read++
			for _, k := range keys {
				if active {
					refs.activeRefs[k] = true
				} else {
					refs.idleRefs[k] = true
				}
			}
		}
	}
	if read == 0 {
		return nil, fmt.Errorf("no checkout of the %d registered projects and repos lists a go.sum, so which module versions are in use is unknown", len(r.Policy.Projects)+len(r.Policy.Repos))
	}
	return refs, nil
}

// goSumZipKeys is the "<escaped path>@<version>" of each go.sum line that names a
// module zip (not the "/go.mod" lines, which need only the tiny .mod file).
func goSumZipKeys(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var keys []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 3 || strings.HasSuffix(fields[1], "/go.mod") {
			continue
		}
		esc, err := module.EscapePath(fields[0])
		if err != nil {
			continue
		}
		keys = append(keys, esc+"@"+fields[1])
	}
	return keys, sc.Err()
}

// classifyModUnit decides how u is protected, if at all.
func classifyModUnit(u *modUnit, refs *modRefs, procs string, open interface{ Holds(string) bool }, cutoff time.Time) {
	u.class = modFree
	switch {
	case !u.lastUse.Before(cutoff):
		u.class, u.why = modPinned, "recent"
	case strings.Contains(procs, u.key) || strings.Contains(procs, unescapedModKey(u.key)):
		u.class, u.why = modPinned, "named by a process"
	case u.dir != "" && open.Holds(u.dir):
		u.class, u.why = modPinned, "open"
	case refs.activeRefs[u.key]:
		u.class, u.why = modPinned, "active checkout"
	case refs.idleRefs[u.key]:
		u.class = modIdleRef
	}
}

// unescapedModKey is key with the "!x" case escapes of module paths undone,
// the spelling a command line carries.
func unescapedModKey(key string) string {
	path, ver, ok := strings.Cut(key, "@")
	if !ok {
		return key
	}
	if p, err := module.UnescapePath(path); err == nil {
		path = p
	}
	return path + "@" + ver
}

// tryLockFile takes the non-blocking exclusive lock on path, the lock cmd/go
// holds while it downloads or extracts that version. A missing file is created,
// as cmd/go would.
func tryLockFile(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return nil, err
	}
	if err := lock(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}
