package storage

import (
	"context"
	"encoding/json"
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
//
// The shared build cache is the layer that matters most and it grows ~40 GB/h
// under agent load, so it runs FIRST and each step gets a guaranteed slice of
// what remains: a slow step cannot starve the next. A step cut off by its
// slice has still reclaimed what it covered (see trimGoBuildCache); that is a
// partial success, never a failure. Only real errors are returned.
func (r Runner) GoCaches(apply bool) error {
	parent := r.hostCtx()
	var failures []error
	steps := []struct {
		name string
		frac float64 // share of the time still left in the slice
		run  func(Runner, bool) error
	}{
		{"shared build cache", 0.6, Runner.SharedGoCache},
		{"golangci-lint cache", 0.5, Runner.GolangciLintCache},
		{"orphaned private caches", 0.5, Runner.OrphanedGoCaches},
		{"goimports index", 1, Runner.GoimportsIndex},
	}
	for _, step := range steps {
		if parent.Err() != nil {
			break
		}
		ctx, cancel := sliceContext(parent, step.frac)
		sub := r
		sub.Ctx = ctx
		err := step.run(sub, apply)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			failures = append(failures, layerErr("go caches: "+step.name, err))
		}
	}
	return errors.Join(failures...)
}

// sliceContext bounds a step to frac of the time its parent has left. A parent
// without a deadline is passed through.
func sliceContext(parent context.Context, frac float64) (context.Context, context.CancelFunc) {
	deadline, ok := parent.Deadline()
	if !ok || frac >= 1 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(float64(time.Until(deadline))*frac))
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

const (
	goCacheShards     = 256
	goCacheSampleSize = 16
)

// goCachePass is what one trim pass did. It is reported, never an error: a pass
// cut off by its deadline that freed space is a partial success.
type goCachePass struct {
	shardsTotal, shardsCovered int
	removed                    int
	freed                      uint64
	finished                   bool
	samples                    []string
}

type goCacheSample struct {
	bytes   uint64
	shards  int
	entries []goCacheEntry // sampled entries older than the floor, oldest first
}

// goCacheShardList returns the cache's shard directories, sorted.
func goCacheShardList(root string) ([]string, error) {
	subs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var shards []string
	for _, sub := range subs {
		if sub.IsDir() && len(sub.Name()) == 2 { // README, trim.txt and the rest are never touched
			shards = append(shards, sub.Name())
		}
	}
	sort.Strings(shards)
	return shards, nil
}

// readGoCacheShard lists one shard's entries. Only stats; never opens a file.
func readGoCacheShard(root, shard string) []goCacheEntry {
	files, err := os.ReadDir(filepath.Join(root, shard))
	if err != nil {
		return nil
	}
	out := make([]goCacheEntry, 0, len(files))
	for _, f := range files {
		if !goCacheEntryName.MatchString(f.Name()) {
			continue
		}
		info, err := f.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, goCacheEntry{shard, f.Name(), info.Size(), info.ModTime()})
	}
	return out
}

// sampleGoCache reads a bounded, evenly spaced subset of shards (starting at
// offset so successive passes sample different ones). Entry hashes are uniform,
// so a shard is a fair 1/256 slice of both size and age distribution.
func sampleGoCache(ctx context.Context, root string, shards []string, offset int, now time.Time) goCacheSample {
	n := goCacheSampleSize
	if n > len(shards) {
		n = len(shards)
	}
	var out goCacheSample
	floor := now.Add(-goCacheFloor)
	for i := 0; i < n && ctx.Err() == nil; i++ {
		shard := shards[(offset+i*len(shards)/n)%len(shards)]
		out.shards++
		for _, e := range readGoCacheShard(root, shard) {
			out.bytes += uint64(e.size)
			if e.mtime.Before(floor) {
				out.entries = append(out.entries, e)
			}
		}
	}
	sort.Slice(out.entries, func(i, j int) bool { return out.entries[i].mtime.Before(out.entries[j].mtime) })
	return out
}

// goCacheCutoff picks the mtime below which entries are deleted: the older of
// "unused longer than the age rule" and "what must go to fit the budget",
// whichever deletes more, and NEVER newer than the safety floor. No global
// sort and no complete scan: the budget cutoff is read off the sample's mtime
// distribution scaled to the whole cache.
func goCacheCutoff(sample goCacheSample, nShards int, now time.Time, unused time.Duration, budget uint64) (cutoff time.Time, estTotal uint64) {
	floorCut := now.Add(-goCacheFloor)
	cutoff = now.Add(-unused)
	if cutoff.After(floorCut) {
		cutoff = floorCut
	}
	if sample.shards == 0 {
		return cutoff, 0
	}
	scale := float64(nShards) / float64(sample.shards)
	estTotal = uint64(float64(sample.bytes) * scale)
	if estTotal <= budget {
		return cutoff, estTotal
	}
	need := float64(estTotal-budget) / scale // bytes to drop from the sample
	var acc float64
	budgetCut := floorCut // the sample cannot cover the excess: take everything past the floor
	for _, e := range sample.entries {
		acc += float64(e.size)
		if acc >= need {
			budgetCut = e.mtime.Add(time.Second) // include this entry itself
			break
		}
	}
	if budgetCut.After(floorCut) {
		budgetCut = floorCut
	}
	if budgetCut.After(cutoff) {
		cutoff = budgetCut
	}
	return cutoff, estTotal
}

// cursorFile persists, per cache, the shard the next pass starts at.
func (r Runner) goCacheCursorPath() string {
	if r.PolicyPath == "" || guardMachinePolicy(r.PolicyPath) != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(r.PolicyPath), "go-cache-cursor.json")
}

func (r Runner) loadGoCacheCursor(label string) int {
	path := r.goCacheCursorPath()
	if path == "" {
		return int(time.Now().Unix()/3600) * 37 // no persistence: rotate by the hour
	}
	var m map[string]int
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &m) == nil {
		return m[label]
	}
	return 0
}

func (r Runner) saveGoCacheCursor(label string, next int) {
	path := r.goCacheCursorPath()
	if path == "" {
		return
	}
	m := map[string]int{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	m[label] = next
	b, _ := json.Marshal(m)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
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
	shards, err := goCacheShardList(root)
	if err != nil {
		return err
	}
	if len(shards) == 0 {
		return nil
	}
	ctx := r.hostCtx()
	now := time.Now()
	start := r.loadGoCacheCursor(label) % len(shards)
	sample := sampleGoCache(ctx, root, shards, start, now)
	cutoff, estTotal := goCacheCutoff(sample, len(shards), now, unused, r.Policy.GoCacheGiB*GiB)

	pass := streamTrimGoCache(ctx, root, shards, start, cutoff, apply)
	if apply {
		r.saveGoCacheCursor(label, (start+pass.shardsCovered)%len(shards))
	}

	r.print("%s %s: ~%.1f GiB estimated (budget %d GiB); deleting entries last used before %s (age rule %s, floor %s)\n",
		label, root, float64(estTotal)/float64(GiB), r.Policy.GoCacheGiB, cutoff.Format(time.RFC3339), unused, goCacheFloor)
	for _, p := range pass.samples {
		r.print("  %s %s\n", verb(apply), p)
	}
	status := "finished"
	if !pass.finished {
		status = fmt.Sprintf("cut off after %d/%d shards; next pass resumes at shard %02x", pass.shardsCovered, pass.shardsTotal, shards[(start+pass.shardsCovered)%len(shards)])
	}
	r.print("%s: %s %d entries, %.1f GiB (%s)\n", label, map[bool]string{true: "removed", false: "would remove"}[apply], pass.removed, float64(pass.freed)/float64(GiB), status)
	return nil
}

// streamTrimGoCache walks the shards in rotated order from start, deleting
// entries older than cutoff as it meets them, so every shard finished has
// already reclaimed its space when a deadline cuts the pass off. Shards are
// worked in parallel; shardsCovered counts the contiguous prefix completed, the
// only part the next pass may skip.
func streamTrimGoCache(ctx context.Context, root string, shards []string, start int, cutoff time.Time, apply bool) goCachePass {
	n := len(shards)
	pass := goCachePass{shardsTotal: n}
	done := make([]bool, n)
	var mu sync.Mutex
	var next, removed, freed atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < goCacheWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				shard := shards[(start+i)%n]
				complete := true
				for _, e := range readGoCacheShard(root, shard) {
					if !e.mtime.Before(cutoff) {
						continue
					}
					if ctx.Err() != nil {
						complete = false
						break
					}
					if apply {
						if err := os.Remove(e.path(root)); err != nil && !errors.Is(err, fs.ErrNotExist) {
							continue
						}
					}
					removed.Add(1)
					freed.Add(e.size)
					mu.Lock()
					if len(pass.samples) < 5 {
						pass.samples = append(pass.samples, fmt.Sprintf("%s (%d bytes)", e.path(root), e.size))
					}
					mu.Unlock()
				}
				if complete {
					mu.Lock()
					done[i] = true
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	for pass.shardsCovered < n && done[pass.shardsCovered] {
		pass.shardsCovered++
	}
	pass.finished = pass.shardsCovered == n
	pass.removed, pass.freed = int(removed.Load()), uint64(freed.Load())
	return pass
}

func verb(apply bool) string {
	if apply {
		return "remove"
	}
	return "would remove"
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

// goCacheStatus reports the shared cache's estimated size from a sample of
// shards (a full scan of a million-entry cache is minutes under load).
func (r Runner) goCacheStatus(ctx context.Context) {
	root, _, err := r.goEnv(ctx)
	if err != nil {
		return
	}
	shards, err := goCacheShardList(root)
	if err != nil || len(shards) == 0 {
		return
	}
	sample := sampleGoCache(ctx, root, shards, 0, time.Now())
	if sample.shards == 0 {
		return
	}
	est := float64(sample.bytes) * float64(len(shards)) / float64(sample.shards)
	r.print("go build cache %s: ~%.1f GiB estimated from %d/%d shards (budget %d GiB, unused %s)\n", root,
		est/float64(GiB), sample.shards, len(shards), r.Policy.GoCacheGiB, r.Policy.GoCacheUnused)
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
