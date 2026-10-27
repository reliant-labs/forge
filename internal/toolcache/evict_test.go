package toolcache

import (
	"context"
	"errors"
	"fmt"
	"github.com/reliant-labs/forge/internal/openfiles"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var evictNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func daysAgo(n int) time.Time { return evictNow.Add(-time.Duration(n) * 24 * time.Hour) }

// installed writes a current-layout entry <root>/<kind>/<key> with its lock
// file and a last-use marker dated used.
func installed(t *testing.T, root, kind, key string, used time.Time) string {
	t.Helper()
	dir := filepath.Join(root, kind, key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, kind, lockDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath(filepath.Join(root, kind), key), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := touch(filepath.Join(dir, UsedMarker), used); err != nil {
		t.Fatal(err)
	}
	return dir
}

// legacy writes a marker-less directory with one file dated at.
func legacy(t *testing.T, dir string, at time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bin, at, at); err != nil {
		t.Fatal(err)
	}
	return dir
}

func notInUse(string) bool { return false }

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestEvict_RemovesUnusedVersionsAndKeepsEachToolsNewest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oldBuf := installed(t, root, KindGate, "buf@v1.50.0+go1.27.2", daysAgo(40))
	newBuf := installed(t, root, KindGate, "buf@v1.57.0+go1.27.3", daysAgo(20)) // stale too, but buf's newest
	recent := installed(t, root, KindPinned, "protoc-gen-go@v1.36.11", daysAgo(3))
	newer := installed(t, root, KindPinned, "protoc-gen-go@v1.36.12", daysAgo(1))
	lintOld := installed(t, root, KindGolangci, "v2.12.0-go1.26.2", daysAgo(30))
	lintNew := installed(t, root, KindGolangci, "v2.14.0-go1.27.2", daysAgo(15))

	got, err := Evict(root, evictNow, EvictPolicy{InUse: notInUse}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gate/buf@v1.50.0+go1.27.2", "golangci-lint/v2.12.0-go1.26.2"}
	if !slices.Equal(got.Removed, want) {
		t.Errorf("Removed = %v; want %v", got.Removed, want)
	}
	for _, gone := range []string{oldBuf, lintOld} {
		if exists(gone) {
			t.Errorf("%s survived", gone)
		}
	}
	for _, kept := range []string{newBuf, recent, newer, lintNew} {
		if !exists(kept) {
			t.Errorf("%s was removed: it is its tool's newest, or recently used", kept)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(root, "*", evictingPrefix+"*")); len(left) != 0 {
		t.Errorf("evicted trees left behind: %v", left)
	}
}

func TestEvict_PreviewRemovesNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	installed(t, root, KindGate, "buf@v1.57.0", daysAgo(1))
	old := installed(t, root, KindGate, "buf@v1.50.0", daysAgo(40))
	var out strings.Builder
	got, err := Evict(root, evictNow, EvictPolicy{InUse: notInUse}, false, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Removed) != 1 || !exists(old) {
		t.Errorf("preview: Removed=%v, entry exists=%v; want it reported and kept", got.Removed, exists(old))
	}
	if !strings.Contains(out.String(), "evict tool cache "+old) {
		t.Errorf("the preview must name the candidate:\n%s", out.String())
	}
}

func TestEvict_KeepsEntriesInUse(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	installed(t, root, KindGate, "buf@v1.57.0", daysAgo(1))
	old := installed(t, root, KindGate, "buf@v1.50.0", daysAgo(40))
	got, err := Evict(root, evictNow, EvictPolicy{InUse: func(p string) bool { return p == old }}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(old) || len(got.Held) != 1 {
		t.Errorf("an entry a process runs from was evicted (held=%v)", got.Held)
	}
}

// A resolver holds the key's shared lock while it checks and touches an
// entry; Evict must not move the entry out from under it.
func TestEvict_KeepsAnEntryBeingResolved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	installed(t, root, KindGate, "buf@v1.57.0", daysAgo(1))
	old := installed(t, root, KindGate, "buf@v1.50.0", daysAgo(40))
	f, err := os.OpenFile(lockPath(filepath.Join(root, KindGate), "buf@v1.50.0"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if ok, err := tryLock(f, false); !ok || err != nil {
		t.Fatalf("take the shared lock: %v %v", ok, err)
	}
	var out strings.Builder
	if _, err := Evict(root, evictNow, EvictPolicy{InUse: notInUse}, true, &out); err != nil {
		t.Fatal(err)
	}
	if !exists(old) || !strings.Contains(out.String(), "being resolved") {
		t.Errorf("an entry under a resolver's lock was evicted:\n%s", out.String())
	}
}

// The decision is re-made under the lock: an entry used since the scan stays.
func TestEvict_RechecksTheLastUseUnderTheLock(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := installed(t, root, KindGate, "buf@v1.50.0", daysAgo(40))
	e := entry{parent: filepath.Join(root, KindGate), name: "buf@v1.50.0", lastUse: daysAgo(40), locked: true}
	if err := touch(filepath.Join(dir, UsedMarker), evictNow); err != nil {
		t.Fatal(err)
	}
	_, kept, err := moveAside(e, evictNow, EvictPolicy{})
	if err != nil || kept == "" || !exists(dir) {
		t.Errorf("kept=%q err=%v exists=%v; an entry touched after the scan must survive", kept, err, exists(dir))
	}
}

// Entries from before the marker are dated by their newest file, and the
// module-path layout older forges used for codegen plugins is maintained too.
func TestEvict_DatesEntriesWithoutAMarkerByTheirFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gateOld := legacy(t, filepath.Join(root, KindGate, "buf@v1.50.0+go1.27.2"), daysAgo(40))
	gateNew := legacy(t, filepath.Join(root, KindGate, "buf@v1.57.0+go1.27.3"), daysAgo(2))
	// Markerless entries still require their stable version-key lock.
	if err := os.MkdirAll(filepath.Join(root, KindGate, lockDir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath(filepath.Join(root, KindGate), "buf@v1.50.0+go1.27.2"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	// The module-path layout has no keep floor (this forge never resolves
	// it): age alone decides.
	modOld := legacy(t, filepath.Join(root, "google.golang.org", "protobuf@v1.36.10"), daysAgo(60))
	modNewest := legacy(t, filepath.Join(root, "google.golang.org", "protobuf@v1.36.12"), daysAgo(50))
	airRecent := legacy(t, filepath.Join(root, "github.com", "air-verse", "air@v1.67.4"), daysAgo(2))

	if _, err := Evict(root, evictNow, EvictPolicy{InUse: notInUse}, true, nil); err != nil {
		t.Fatal(err)
	}
	if exists(gateOld) {
		t.Errorf("%s survived", gateOld)
	}
	// Legacy installs without a provable version lock must fail closed too.
	for _, kept := range []string{gateNew, airRecent, modOld, modNewest} {
		if !exists(kept) {
			t.Errorf("%s was removed: it is its tool's newest, or recently installed", kept)
		}
	}
}

func TestEvict_KeepsWhatItCannotDate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	empty := filepath.Join(root, KindGate, "buf@v1.40.0")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	installed(t, root, KindGate, "buf@v1.57.0", daysAgo(1))
	if _, err := Evict(root, evictNow, EvictPolicy{InUse: notInUse}, true, nil); err != nil {
		t.Fatal(err)
	}
	if !exists(empty) {
		t.Error("an entry with no marker and no files was removed; it cannot be dated")
	}
}

// What an interrupted eviction or an abandoned install left is cleared; an
// install still running (fresh staging) is not.
func TestEvict_ClearsLeftoversButNotARunningInstall(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	kindDir := filepath.Join(root, KindGate)
	trash := legacy(t, filepath.Join(kindDir, evictingPrefix+"buf@v1.0.0-1"), evictNow)
	abandoned := filepath.Join(kindDir, stagingPrefix+"buf@v1.1.0-123")
	running := filepath.Join(kindDir, stagingPrefix+"buf@v1.2.0-456")
	for _, key := range []string{"buf@v1.0.0", "buf@v1.1.0", "buf@v1.2.0"} {
		installed(t, root, KindGate, key, evictNow)
	}
	for _, d := range []string{abandoned, running} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(abandoned, daysAgo(3), daysAgo(3)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(running, evictNow, evictNow); err != nil {
		t.Fatal(err)
	}
	if _, err := Evict(root, evictNow, EvictPolicy{InUse: notInUse}, true, nil); err != nil {
		t.Fatal(err)
	}
	if exists(trash) || exists(abandoned) {
		t.Error("leftovers survived")
	}
	if !exists(running) {
		t.Error("a running install's staging dir was removed")
	}
}

// The directory timestamp cannot prove a build has stopped. Even very old
// stages must take the exact version key lock and pass process inspection.
func TestEvict_LeftoversRequireLockAndProcessEvidence(t *testing.T) {
	for _, mode := range []string{"locked", "in-use", "inspection-failed", "unknown", "missing-lock"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			key := "buf@v1.2.3+go1.27.3"
			installed(t, root, KindGate, key, evictNow)
			name := stagingPrefix + key + "-1234"
			if mode == "unknown" {
				name = ".unrecognized-old-state"
			}
			path := legacy(t, filepath.Join(root, KindGate, name), daysAgo(4))
			if err := os.Chtimes(path, daysAgo(4), daysAgo(4)); err != nil {
				t.Fatal(err)
			}
			policy := EvictPolicy{InUse: notInUse}
			switch mode {
			case "locked":
				f, err := os.OpenFile(lockPath(filepath.Join(root, KindGate), key), os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = f.Close() }()
				if ok, err := tryLock(f, true); !ok || err != nil {
					t.Fatalf("lock: %v %v", ok, err)
				}
			case "in-use":
				policy.InUse = func(p string) bool { return p == path }
			case "inspection-failed":
				policy.InUse = nil
				policy.inspect = func(context.Context) (openfiles.Snapshot, error) {
					return openfiles.Snapshot{}, errors.New("inspection denied")
				}
			case "missing-lock":
				if err := os.Remove(lockPath(filepath.Join(root, KindGate), key)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Evict(root, evictNow, policy, true, nil); err != nil {
				t.Fatal(err)
			}
			if !exists(path) {
				t.Fatal("uncertain/live leftover removed")
			}
		})
	}
}

func TestEvict_InspectionFailureRetainsAllRemovalPaths(t *testing.T) {
	root := t.TempDir()
	key := "buf@v1"
	old := installed(t, root, KindGate, key, daysAgo(40))
	installed(t, root, KindGate, "buf@v2", evictNow)
	stage := legacy(t, filepath.Join(root, KindGate, stagingPrefix+key+"-123"), daysAgo(40))
	trash := legacy(t, filepath.Join(root, KindGate, evictingPrefix+key+"-456"), daysAgo(40))
	if err := os.Chtimes(stage, daysAgo(40), daysAgo(40)); err != nil {
		t.Fatal(err)
	}
	policy := EvictPolicy{inspect: func(context.Context) (openfiles.Snapshot, error) {
		return openfiles.Snapshot{}, errors.New("inspection denied")
	}}
	if _, err := Evict(root, evictNow, policy, true, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{old, stage, trash} {
		if !exists(p) {
			t.Fatalf("removed %s despite failed inspection", p)
		}
	}
}

func TestReplacementCleanupPreservesOutsideTarget(t *testing.T) {
	for _, entryLink := range []bool{false, true} {
		t.Run(fmt.Sprint(entryLink), func(t *testing.T) {
			root := t.TempDir()
			outside := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(outside, []byte("outside"), 0400); err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(root, "fake@v1")
			if entryLink {
				if err := os.Symlink(filepath.Dir(outside), entry); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(entry, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(entry, "stray")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Ensure(context.Background(), root, "fake@v1", readyWith("fixed"), writeTool("fixed")); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(outside)
			if err != nil || info.Mode().Perm() != 0400 {
				t.Fatalf("replacement chmod escaped: %v %v", info, err)
			}
			if b, err := os.ReadFile(outside); err != nil || string(b) != "outside" {
				t.Fatalf("replacement delete escaped: %q %v", b, err)
			}
		})
	}
}

func TestEvict_InUseEvictionLeftoverSurvives(t *testing.T) {
	root := t.TempDir()
	key := "buf@v1"
	installed(t, root, KindGate, key, evictNow)
	path := legacy(t, filepath.Join(root, KindGate, evictingPrefix+key+"-123"), daysAgo(40))
	if _, err := Evict(root, evictNow, EvictPolicy{InUse: func(p string) bool { return p == path }}, true, nil); err != nil {
		t.Fatal(err)
	}
	if !exists(path) {
		t.Fatal("mapped executable/open eviction leftover removed")
	}
}

func TestCleanupNeverChangesOutsideSymlinkTargets(t *testing.T) {
	for _, shape := range []string{"child-file", "child-dir", "entry-link", "ancestor-link"} {
		t.Run(shape, func(t *testing.T) {
			base := t.TempDir()
			outside := filepath.Join(base, "outside")
			if err := os.Mkdir(outside, 0755); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(outside, 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(outside, 0700) })
			parent := filepath.Join(base, "cache")
			if err := os.Mkdir(parent, 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "rejected")
			switch shape {
			case "entry-link":
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "ancestor-link":
				if err := os.Symlink(outside, filepath.Join(parent, "alias")); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(parent, "alias", "sentinel")
			default:
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
				target := sentinel
				if shape == "child-dir" {
					target = outside
				}
				if err := os.Symlink(target, filepath.Join(path, "stray")); err != nil {
					t.Fatal(err)
				}
			}
			_ = removeAllWritable(path) // confinement refusal is correct for an ancestor link
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "untouched" {
				t.Fatalf("sentinel bytes changed: %q %v", data, err)
			}
			info, err := os.Stat(sentinel)
			if err != nil || info.Mode().Perm() != 0400 {
				t.Fatalf("sentinel mode changed: %v %v", info, err)
			}
			info, err = os.Stat(outside)
			if err != nil || info.Mode().Perm() != 0500 {
				t.Fatalf("outside directory mode changed: %v %v", info, err)
			}
		})
	}
}

func TestEvict_SymlinkedRootOrChangedRootRetained(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "cache")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	installed(t, root, KindGate, "buf@v1", daysAgo(40))
	installed(t, root, KindGate, "buf@v2", evictNow)
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Evict(alias, evictNow, EvictPolicy{InUse: notInUse}, true, nil); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(root, KindGate, "buf@v1")) {
		t.Fatal("symlinked root traversed")
	}
	anchor, err := openCacheDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer anchor.close()
	moved := root + "-old"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	err = (EvictPolicy{}).removeConfined(root, anchor, filepath.Join(root, KindGate, "buf@v1"))
	if err == nil {
		t.Fatal("root inode replacement accepted")
	}
	if !exists(filepath.Join(moved, KindGate, "buf@v1")) {
		t.Fatal("original root altered after replacement")
	}
}

func TestEvict_MissingRootIsNothingToDo(t *testing.T) {
	t.Parallel()
	got, err := Evict(filepath.Join(t.TempDir(), "absent"), evictNow, EvictPolicy{}, true, nil)
	if err != nil || len(got.Removed) != 0 {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestEvict_HonoursKeepAndMaxAge(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := installed(t, root, KindGate, "buf@v1", daysAgo(10))
	b := installed(t, root, KindGate, "buf@v2", daysAgo(9))
	c := installed(t, root, KindGate, "buf@v3", daysAgo(8))
	_, err := Evict(root, evictNow, EvictPolicy{InUse: notInUse, Keep: 2, MaxAge: 7 * 24 * time.Hour}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if exists(a) || !exists(b) || !exists(c) {
		t.Errorf("keep 2 / max age 7d: v1 exists=%v v2=%v v3=%v; want only v1 removed", exists(a), exists(b), exists(c))
	}
}
