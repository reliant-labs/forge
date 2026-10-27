package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A `go run` or `go tool` executable lives in the build cache, in an
// executable entry (<root>/xx/<64 hex>-d/<name>), and the process running it
// is not a Go toolchain process: `go run`'s child shows up as `forge`, not as
// `go`. The age lease (gocache_livebuild.go) reaches that entry only through
// the waiting parent `go`, which is not counted once it is older than
// goCacheLongLived, and not at all when nothing waits (buf runs forge's own
// path as a plugin; a delegate re-execs it). Deleting the entry then makes
// the live process fail the moment it re-opens its own path.
//
// So the trim also asks the kernel which executables are running, and:
//
//   - never removes an executable entry that is some live process's
//     executable, whatever its age (a snapshot is taken per pass);
//   - counts such a process as a running Go build for the cutoff's lease;
//   - when the snapshot cannot be taken, removes no executable entry at all.
//
// An unreadable process table protects every `-d` entry for the pass. That
// costs space for one pass; guessing would cost a running process its binary.

// LiveExecutable is the executable of one running process.
type LiveExecutable struct {
	// Path is the process's executable (/proc/<pid>/exe), without the
	// " (deleted)" suffix the kernel appends once the file is unlinked.
	Path string
	// Age is how long the process has run.
	Age time.Duration
}

// liveExecutables returns the executables of running processes that live in
// the cache at root. root is resolved through symlinks, as the kernel reports
// resolved paths.
func (r Runner) liveExecutables(ctx context.Context, root string) ([]LiveExecutable, error) {
	list := r.LiveExecutables
	if list == nil {
		if testing.Testing() {
			return nil, nil
		}
		list = procExecutables
	}
	all, err := list(ctx, root)
	if err != nil {
		return nil, err
	}
	resolved := root
	if p, err := filepath.EvalSymlinks(root); err == nil {
		resolved = p
	}
	var inCache []LiveExecutable
	for _, e := range all {
		if _, ok := cacheEntryOfExecutable(resolved, e.Path); ok {
			inCache = append(inCache, e)
		} else if _, ok := cacheEntryOfExecutable(root, e.Path); ok {
			inCache = append(inCache, e)
		}
	}
	return inCache, nil
}

// cacheEntryOfExecutable names the executable entry ("xx/<hash>-d") that exe
// is inside, if exe is the binary of one under root.
func cacheEntryOfExecutable(root, exe string) (string, bool) {
	exe = strings.TrimSuffix(exe, " (deleted)")
	rel, err := filepath.Rel(root, exe)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 3 || !goCacheEntryName.MatchString(parts[1]) || !strings.HasSuffix(parts[1], "-d") {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

// liveExecutableGuard says which entries of the cache at root a running
// process holds. A nil guard protects nothing.
type liveExecutableGuard struct {
	// entries are the "xx/<hash>-d" names of executable entries in use.
	entries map[string]bool
	// all protects every executable entry: the snapshot could not be taken.
	all bool
	// ages are the ages of the processes running from the cache.
	ages []time.Duration
}

// newLiveExecutableGuard takes the pass's snapshot of running executables.
func (r Runner) newLiveExecutableGuard(ctx context.Context, root string) *liveExecutableGuard {
	exes, err := r.liveExecutables(ctx, root)
	if err != nil {
		r.print("go build cache: cannot list running executables (%v); keeping every executable entry (-d) this pass\n", err)
		return &liveExecutableGuard{all: true}
	}
	g := &liveExecutableGuard{entries: map[string]bool{}}
	resolved := root
	if p, err := filepath.EvalSymlinks(root); err == nil {
		resolved = p
	}
	for _, e := range exes {
		key, ok := cacheEntryOfExecutable(resolved, e.Path)
		if !ok {
			key, ok = cacheEntryOfExecutable(root, e.Path)
		}
		if !ok {
			continue
		}
		g.entries[key] = true
		g.ages = append(g.ages, e.Age)
	}
	return g
}

// holds reports whether the trim must keep e.
func (g *liveExecutableGuard) holds(e goCacheEntry) bool {
	if g == nil || !e.dir {
		return false
	}
	return g.all || g.entries[e.sub+"/"+e.name]
}

// describe is the pass's one-line account of what the guard kept, or "".
func (g *liveExecutableGuard) describe() string {
	switch {
	case g == nil:
		return ""
	case g.all:
		return "; every executable entry kept (running executables could not be listed)"
	case len(g.entries) > 0:
		return fmt.Sprintf("; %d executable entries of running processes kept", len(g.entries))
	}
	return ""
}

// protectedBytes estimates the bytes of the whole cache the guard keeps past
// cutoff, from the sample (whose entries are those past the floor).
func (g *liveExecutableGuard) protectedBytes(sample goCacheSample, nShards int, cutoff time.Time) uint64 {
	if g == nil || sample.shards == 0 {
		return 0
	}
	var n uint64
	for _, e := range sample.entries {
		if e.mtime.Before(cutoff) && g.holds(e) {
			n += uint64(e.size)
		}
	}
	return uint64(float64(n) * float64(nShards) / float64(sample.shards))
}
