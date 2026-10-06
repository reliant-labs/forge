package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Go-toolchain cache layer: the shared build cache, the golangci-lint cache,
// the goimports module index, and orphaned private caches (gocache_orphans.go).
//
// Why age-based trim and not `go clean -cache`: without -trimpath Go keys every
// main-module package by its absolute directory, so each worktree gets its own
// full copy of the cache. Go's built-in trim (entries unused for 5 days) cannot
// keep up. Wiping the cache under running builds produces
// `link: cannot open file …/go-build/…-d`. Trimming by mtime is safe while
// builds run because Go bumps an entry's mtime on use whenever it is more than
// an hour stale: an entry older than X has been unused for at least X-1h, so
// with a floor of 2h a live build never loses an entry it is using.

// goCacheFloor is the youngest entry the trim may ever delete, whatever the
// policy says. It is what makes the trim safe under live builds.
const goCacheFloor = 2 * time.Hour

// goCacheWorkers bounds the parallel unlinks. Deleting millions of small
// files is latency-bound, not bandwidth-bound.
const goCacheWorkers = 16

var goCacheEntryName = regexp.MustCompile(`^[0-9a-f]{64}-[ad]$`)

// goBuildCacheSentinelPrefix is the first line shared by Go's cache README and
// golangci-lint's fork of it.
const goBuildCacheSentinelPrefix = "This directory holds cached build artifacts from"

type goCacheEntry struct {
	sub   string
	name  string
	size  int64
	mtime time.Time
}

func (e goCacheEntry) path(root string) string { return filepath.Join(root, e.sub, e.name) }

// GoCaches trims every Go-toolchain cache. Dry-run unless apply is set.
// Layers run independently; one failure does not stop the rest.
func (r Runner) GoCaches(apply bool) error {
	var failures []error
	steps := []struct {
		name string
		run  func(bool) error
	}{
		{"orphaned private caches", r.OrphanedGoCaches},
		{"shared build cache", r.SharedGoCache},
		{"golangci-lint cache", r.GolangciLintCache},
		{"goimports index", r.GoimportsIndex},
	}
	for _, step := range steps {
		if err := r.hostCtx().Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := step.run(apply); err != nil {
			failures = append(failures, layerErr("go caches: "+step.name, err))
		}
	}
	return errors.Join(failures...)
}

func refuseUnderTest(field string) error {
	return fmt.Errorf("storage.Runner.%s is unset under test: set it to a t.TempDir() "+
		"(an unset root would trim the developer's real Go cache)", field)
}

// goEnv resolves the shared GOCACHE and GOMODCACHE.
func (r Runner) goEnv(ctx context.Context) (gocache, gomodcache string, err error) {
	gocache, gomodcache = r.GoCacheRoot, r.GoModCacheRoot
	if gocache != "" && gomodcache != "" {
		return gocache, gomodcache, nil
	}
	if testing.Testing() {
		if gocache == "" {
			return "", "", refuseUnderTest("GoCacheRoot")
		}
		return gocache, "", nil
	}
	out, err := r.command(ctx, "go", "env", "GOCACHE", "GOMODCACHE")
	if err != nil {
		return "", "", fmt.Errorf("go env: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || strings.TrimSpace(lines[0]) == "" || strings.TrimSpace(lines[1]) == "" {
		return "", "", fmt.Errorf("unexpected `go env` output %q", out)
	}
	if gocache == "" {
		gocache = strings.TrimSpace(lines[0])
	}
	if gomodcache == "" {
		gomodcache = strings.TrimSpace(lines[1])
	}
	return gocache, gomodcache, nil
}

// SharedGoCache trims the shared `go env GOCACHE`.
func (r Runner) SharedGoCache(apply bool) error {
	root := r.GoCacheRoot
	if root == "" {
		var err error
		if root, _, err = r.goEnv(r.hostCtx()); err != nil {
			r.print("skip shared go build cache (%v)\n", err)
			return nil
		}
	}
	return r.trimGoBuildCache("shared go build cache", root, apply)
}

// GolangciLintCache trims golangci-lint's fork of the Go cache format.
func (r Runner) GolangciLintCache(apply bool) error {
	root := r.GolangciCacheRoot
	if root == "" {
		if testing.Testing() {
			r.print("skip golangci-lint cache (%v)\n", refuseUnderTest("GolangciCacheRoot"))
			return nil
		}
		if env := os.Getenv("GOLANGCI_LINT_CACHE"); env != "" {
			root = env
		} else if dir, err := os.UserCacheDir(); err == nil {
			root = filepath.Join(dir, "golangci-lint")
		} else {
			return nil
		}
	}
	return r.trimGoBuildCache("golangci-lint cache", root, apply)
}

// hasCacheSentinel reports whether root's README is a Go-style cache marker.
func hasCacheSentinel(root string) bool {
	f, err := os.Open(filepath.Join(root, "README"))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, len(goBuildCacheSentinelPrefix))
	n, _ := f.Read(buf)
	return string(buf[:n]) == goBuildCacheSentinelPrefix
}

func (r Runner) trimGoBuildCache(label, root string, apply bool) error {
	if _, err := os.Stat(root); err != nil {
		return nil // absent: nothing to trim
	}
	if !hasCacheSentinel(root) {
		r.print("skip %s %s: no cache README, not recognised as a Go cache\n", label, root)
		return nil
	}
	unused, err := time.ParseDuration(r.Policy.GoCacheUnused)
	if err != nil {
		return fmt.Errorf("go_cache_unused: %w", err)
	}
	if unused < goCacheFloor {
		unused = goCacheFloor
	}
	ctx := r.hostCtx()
	entries, complete, err := scanGoCache(ctx, root)
	if err != nil {
		return err
	}
	victims, total, reclaim, over := selectGoCacheVictims(entries, time.Now(), unused, r.Policy.GoCacheGiB*GiB, complete)
	r.print("%s %s: %.1f GiB in %d entries; %d unused over %s (%.1f GiB)", label, root,
		float64(total)/float64(GiB), len(entries), len(victims)-over, unused, float64(reclaim-overBytes(victims, over))/float64(GiB))
	if over > 0 {
		r.print(", plus %d oldest to fit the %d GiB budget (%.1f GiB)", over, r.Policy.GoCacheGiB, float64(overBytes(victims, over))/float64(GiB))
	}
	if !complete {
		r.print(" [scan cut off by deadline; budget trim skipped]")
	}
	r.print("\n")
	// Per-entry lines would be millions; show the oldest few with their bytes.
	for i, e := range victims {
		if i == 5 {
			r.print("  … %d more entries\n", len(victims)-5)
			break
		}
		r.print("  %s %s (%d bytes)\n", verb(apply), e.path(root), e.size)
	}
	if !apply || len(victims) == 0 {
		return nil
	}
	removed, freed := removeGoCacheEntries(ctx, root, victims)
	r.print("%s: removed %d entries, %.1f GiB", label, removed, float64(freed)/float64(GiB))
	if removed < len(victims) {
		r.print(" (stopped early; remaining entries are retained for the next pass)")
	}
	r.print("\n")
	return nil
}

func verb(apply bool) string {
	if apply {
		return "remove"
	}
	return "would remove"
}

func overBytes(victims []goCacheEntry, over int) uint64 {
	var n uint64
	for _, e := range victims[len(victims)-over:] {
		n += uint64(e.size)
	}
	return n
}

// scanGoCache lists every cache entry, one worker per fan-out slot across the
// 256 shard directories. complete is false when ctx ended first.
func scanGoCache(ctx context.Context, root string) (entries []goCacheEntry, complete bool, err error) {
	subs, err := os.ReadDir(root)
	if err != nil {
		return nil, false, err
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var cut atomic.Bool
	work := make(chan string)
	for w := 0; w < goCacheWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sub := range work {
				files, err := os.ReadDir(filepath.Join(root, sub))
				if err != nil {
					continue
				}
				var local []goCacheEntry
				for _, f := range files {
					if !goCacheEntryName.MatchString(f.Name()) {
						continue
					}
					info, err := f.Info()
					if err != nil || !info.Mode().IsRegular() {
						continue
					}
					local = append(local, goCacheEntry{sub, f.Name(), info.Size(), info.ModTime()})
				}
				mu.Lock()
				entries = append(entries, local...)
				mu.Unlock()
			}
		}()
	}
	for _, sub := range subs {
		if ctx.Err() != nil {
			cut.Store(true)
			break
		}
		if !sub.IsDir() || len(sub.Name()) != 2 {
			continue // README, trim.txt, testexpire.txt and anything else: never touched
		}
		work <- sub.Name()
	}
	close(work)
	wg.Wait()
	return entries, !cut.Load() && ctx.Err() == nil, nil
}

// selectGoCacheVictims returns the entries to delete, oldest first. over is
// how many trailing victims were chosen only to fit the budget.
func selectGoCacheVictims(entries []goCacheEntry, now time.Time, unused time.Duration, budget uint64, complete bool) (victims []goCacheEntry, total, reclaim uint64, over int) {
	cutoff := now.Add(-unused)
	floor := now.Add(-goCacheFloor)
	var young []goCacheEntry
	for _, e := range entries {
		total += uint64(e.size)
		if e.mtime.Before(cutoff) {
			victims = append(victims, e)
			reclaim += uint64(e.size)
		} else if e.mtime.Before(floor) {
			young = append(young, e)
		}
	}
	byAge := func(s []goCacheEntry) {
		sort.Slice(s, func(i, j int) bool { return s[i].mtime.Before(s[j].mtime) })
	}
	byAge(victims)
	if complete && total-reclaim > budget {
		byAge(young)
		for _, e := range young {
			if total-reclaim <= budget {
				break
			}
			victims = append(victims, e)
			reclaim += uint64(e.size)
			over++
		}
	}
	return victims, total, reclaim, over
}

// removeGoCacheEntries unlinks victims in order across a worker pool and
// stops cleanly when ctx ends.
func removeGoCacheEntries(ctx context.Context, root string, victims []goCacheEntry) (removed int, freed uint64) {
	var next, nRemoved, nFreed atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < goCacheWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				i := int(next.Add(1)) - 1
				if i >= len(victims) {
					return
				}
				err := os.Remove(victims[i].path(root))
				if err == nil || errors.Is(err, fs.ErrNotExist) {
					nRemoved.Add(1)
					nFreed.Add(victims[i].size)
				}
			}
		}()
	}
	wg.Wait()
	return int(nRemoved.Load()), uint64(nFreed.Load())
}

// ---- goimports module index -------------------------------------------------

var goimportsPayload = regexp.MustCompile(`^index-[0-9]+-[0-9]+$`)

const goimportsKeepRecent = 24 * time.Hour

// GoimportsIndex prunes stale goimports/gopls module-index generations.
//
// x/tools/internal/modindex writes each index to a fresh randomly named
// payload (index-<ver>-<rand>) and then points a predictable link file
// (index-name-<ver>-<sha256 of GOMODCACHE>) at it by name; Read follows the
// link and nothing prunes old payloads. So the current generations are exactly
// the payload names held by link files. Those, and anything touched in the
// last 24h (a writer between "payload written" and "link updated"), are kept.
// If no link can be read the current generation is not certain, so the newest
// two payloads are kept as well.
func (r Runner) GoimportsIndex(apply bool) error {
	root := r.GoimportsRoot
	if root == "" {
		if testing.Testing() {
			r.print("skip goimports index (%v)\n", refuseUnderTest("GoimportsRoot"))
			return nil
		}
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil
		}
		root = filepath.Join(dir, "goimports")
	}
	files, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	type payload struct {
		name  string
		size  int64
		mtime time.Time
	}
	var payloads []payload
	keep := map[string]bool{}
	linksOK := true
	links := 0
	for _, f := range files {
		switch {
		case strings.HasPrefix(f.Name(), "index-name-"):
			links++
			b, err := os.ReadFile(filepath.Join(root, f.Name()))
			if err != nil || len(b) == 0 {
				linksOK = false
				continue
			}
			keep[strings.TrimSpace(string(b))] = true
		case goimportsPayload.MatchString(f.Name()):
			info, err := f.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			payloads = append(payloads, payload{f.Name(), info.Size(), info.ModTime()})
		}
	}
	sort.Slice(payloads, func(i, j int) bool { return payloads[i].mtime.After(payloads[j].mtime) })
	if !linksOK || links == 0 {
		for i := 0; i < len(payloads) && i < 2; i++ {
			keep[payloads[i].name] = true
		}
	}
	now := time.Now()
	var reclaim int64
	var victims []payload
	for i := len(payloads) - 1; i >= 0; i-- { // oldest first
		p := payloads[i]
		if keep[p.name] || now.Sub(p.mtime) < goimportsKeepRecent {
			continue
		}
		victims = append(victims, p)
		reclaim += p.size
	}
	r.print("goimports index %s: %d generations, %d stale (%.1f GiB)\n", root, len(payloads), len(victims), float64(reclaim)/float64(GiB))
	ctx := r.hostCtx()
	removed := 0
	for _, p := range victims {
		if ctx.Err() != nil {
			r.print("goimports index: stopped early at the deadline; %d stale generations retained\n", len(victims)-removed)
			return nil
		}
		r.print("  %s %s (%d bytes)\n", verb(apply), filepath.Join(root, p.name), p.size)
		if apply {
			if err := os.Remove(filepath.Join(root, p.name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		removed++
	}
	return nil
}

// goCacheStatus reports the Go caches' sizes. Read-only; skipped under test
// unless roots are injected.
func (r Runner) goCacheStatus(ctx context.Context) {
	root, _, err := r.goEnv(ctx)
	if err != nil {
		return
	}
	if entries, _, err := scanGoCache(ctx, root); err == nil {
		var total uint64
		for _, e := range entries {
			total += uint64(e.size)
		}
		r.print("go build cache %s: %.1f GiB in %d entries (budget %d GiB, unused %s)\n", root,
			float64(total)/float64(GiB), len(entries), r.Policy.GoCacheGiB, r.Policy.GoCacheUnused)
	}
}

// ProductionGoCacheRoots resolves the machine's Go-toolchain cache locations
// for a Runner: the shared GOCACHE/GOMODCACHE (from `go env`), the
// golangci-lint cache and the goimports index. Anything that cannot be located
// is left empty, which the layers skip rather than guess.
func ProductionGoCacheRoots(ctx context.Context, command func(context.Context, string, ...string) ([]byte, error)) (gocache, gomodcache, golangci, goimports string) {
	if out, err := command(ctx, "go", "env", "GOCACHE", "GOMODCACHE"); err == nil {
		if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); len(lines) == 2 {
			gocache, gomodcache = strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
		}
	}
	if env := os.Getenv("GOLANGCI_LINT_CACHE"); env != "" {
		golangci = env
	}
	if dir, err := os.UserCacheDir(); err == nil {
		if golangci == "" {
			golangci = filepath.Join(dir, "golangci-lint")
		}
		goimports = filepath.Join(dir, "goimports")
	}
	return
}
