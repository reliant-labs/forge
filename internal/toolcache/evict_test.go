package toolcache

import (
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
	// The module-path layout has no keep floor (this forge never resolves
	// it): age alone decides.
	modOld := legacy(t, filepath.Join(root, "google.golang.org", "protobuf@v1.36.10"), daysAgo(60))
	modNewest := legacy(t, filepath.Join(root, "google.golang.org", "protobuf@v1.36.12"), daysAgo(50))
	airRecent := legacy(t, filepath.Join(root, "github.com", "air-verse", "air@v1.67.4"), daysAgo(2))

	if _, err := Evict(root, evictNow, EvictPolicy{InUse: notInUse}, true, nil); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{gateOld, modOld, modNewest} {
		if exists(gone) {
			t.Errorf("%s survived", gone)
		}
	}
	for _, kept := range []string{gateNew, airRecent} {
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
	abandoned := filepath.Join(kindDir, stagingPrefix+"buf@v1.1.0-abc")
	running := filepath.Join(kindDir, stagingPrefix+"buf@v1.2.0-def")
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
