package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/commitpolicy"
	"github.com/reliant-labs/forge/internal/openfiles"
)

func hexName(n int, suffix string) string {
	return strings.Repeat("a", 63) + string(rune('0'+n%10)) + suffix
}

func makeBuildCache(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(root, "README")
	if err := os.WriteFile(readme, []byte(commitpolicy.GoBuildCacheSentinel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-1000 * time.Hour)
	_ = os.Chtimes(readme, at, at)
}

func putEntry(t *testing.T, root, sub, name string, size int, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(root, sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func goRunner(t *testing.T, out *strings.Builder) Runner {
	p := DefaultPolicy()
	return Runner{Policy: p, Out: out, Ctx: context.Background()}
}

func TestSharedGoCacheAgeAndFloor(t *testing.T) {
	root := t.TempDir()
	makeBuildCache(t, root)
	old := putEntry(t, root, "ab", hexName(1, "-a"), 10, 72*time.Hour)
	mid := putEntry(t, root, "ab", hexName(2, "-d"), 10, 10*time.Hour)
	fresh := putEntry(t, root, "cd", hexName(3, "-a"), 10, 30*time.Minute)
	trim := filepath.Join(root, "trim.txt")
	if err := os.WriteFile(trim, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-1000 * time.Hour)
	_ = os.Chtimes(trim, at, at)
	junk := putEntry(t, root, "ab", "notanentry", 5, 1000*time.Hour)

	var out strings.Builder
	r := goRunner(t, &out)
	r.GoCacheRoot = root
	if err := r.SharedGoCache(false); err != nil {
		t.Fatal(err)
	}
	if !exists(old) || !strings.Contains(out.String(), "would remove") {
		t.Fatalf("dry-run must list but not delete:\n%s", out.String())
	}
	if err := r.SharedGoCache(true); err != nil {
		t.Fatal(err)
	}
	if exists(old) || !exists(mid) || !exists(fresh) || !exists(trim) || !exists(junk) {
		t.Fatalf("old=%v mid=%v fresh=%v trim=%v junk=%v", exists(old), exists(mid), exists(fresh), exists(trim), exists(junk))
	}
}

func TestSelectGoCacheVictimsBudgetRespectsFloor(t *testing.T) {
	now := time.Now()
	e := func(age time.Duration, size int64) goCacheEntry {
		return goCacheEntry{size: size, mtime: now.Add(-age)}
	}
	entries := []goCacheEntry{e(10*time.Hour, 100), e(5*time.Hour, 100), e(3*time.Hour, 100), e(time.Hour, 100), e(time.Minute, 100)}
	v, _, _, over := selectGoCacheVictims(entries, now, 48*time.Hour, 100, true)
	// total 500, budget 100: only the three entries older than 2h are eligible.
	if len(v) != 3 || over != 3 {
		t.Fatalf("victims=%d over=%d", len(v), over)
	}
	if !v[0].mtime.Before(v[1].mtime) {
		t.Fatal("must be oldest first")
	}
	// An incomplete scan never budget-trims.
	if v, _, _, _ = selectGoCacheVictims(entries, now, 48*time.Hour, 100, false); len(v) != 0 {
		t.Fatalf("incomplete scan trimmed %d", len(v))
	}
}

func TestSharedGoCacheUnsetRefusedUnderTest(t *testing.T) {
	var out strings.Builder
	r := goRunner(t, &out)
	if err := r.SharedGoCache(true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unset under test") {
		t.Fatalf("expected refusal: %q", out.String())
	}
}

func TestSharedGoCacheNeedsSentinel(t *testing.T) {
	root := t.TempDir()
	old := putEntry(t, root, "ab", hexName(1, "-a"), 10, 72*time.Hour)
	var out strings.Builder
	r := goRunner(t, &out)
	r.GoCacheRoot = root
	if err := r.SharedGoCache(true); err != nil {
		t.Fatal(err)
	}
	if !exists(old) {
		t.Fatal("a directory without the cache README must not be trimmed")
	}
}

func TestGolangciLintCache(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("This directory holds cached build artifacts from golangci-lint.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := putEntry(t, root, "00", hexName(1, "-a"), 10, 72*time.Hour)
	fresh := putEntry(t, root, "00", hexName(2, "-a"), 10, time.Minute)
	var out strings.Builder
	r := goRunner(t, &out)
	r.GolangciCacheRoot = root
	if err := r.GolangciLintCache(true); err != nil {
		t.Fatal(err)
	}
	if exists(old) || !exists(fresh) {
		t.Fatal("golangci-lint cache not trimmed correctly")
	}
}

func TestGoimportsIndexKeepsLinkedAndRecent(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		_ = os.Chtimes(p, at, at)
		return p
	}
	current := mk("index-0-1", 500*time.Hour) // old but linked
	stale := mk("index-0-2", 500*time.Hour)
	recent := mk("index-0-3", time.Hour)
	if err := os.WriteFile(filepath.Join(root, "index-name-0-abc"), []byte("index-0-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	r := goRunner(t, &out)
	r.GoimportsRoot = root
	if err := r.GoimportsIndex(false); err != nil || !exists(stale) {
		t.Fatalf("dry-run deleted: %v", err)
	}
	if err := r.GoimportsIndex(true); err != nil {
		t.Fatal(err)
	}
	if exists(stale) || !exists(current) || !exists(recent) {
		t.Fatalf("stale=%v current=%v recent=%v", exists(stale), exists(current), exists(recent))
	}
}

func TestGoimportsIndexWithoutLinksKeepsNewestTwo(t *testing.T) {
	root := t.TempDir()
	var paths []string
	for i, age := range []time.Duration{900, 800, 700, 600} {
		p := filepath.Join(root, "index-0-"+string(rune('1'+i)))
		_ = os.WriteFile(p, []byte("x"), 0o600)
		at := time.Now().Add(-age * time.Hour)
		_ = os.Chtimes(p, at, at)
		paths = append(paths, p)
	}
	var out strings.Builder
	r := goRunner(t, &out)
	r.GoimportsRoot = root
	if err := r.GoimportsIndex(true); err != nil {
		t.Fatal(err)
	}
	if exists(paths[0]) || exists(paths[1]) || !exists(paths[2]) || !exists(paths[3]) {
		t.Fatal("expected only the newest two kept")
	}
}

// orphan fixtures

type orphanFixture struct {
	scan, shared, orphan, young, live, held string
	r                                       Runner
	out                                     *strings.Builder
}

func newOrphanFixture(t *testing.T, procText string, open []string, procErr error) orphanFixture {
	t.Helper()
	scan := t.TempDir()
	scan = resolved(t, scan)
	f := orphanFixture{scan: scan, out: &strings.Builder{}}
	f.shared = filepath.Join(scan, "shared-gocache")
	makeBuildCache(t, f.shared)
	putEntry(t, f.shared, "ab", hexName(1, "-a"), 10, 100*time.Hour)

	f.orphan = filepath.Join(scan, ".gocache-old")
	makeBuildCache(t, f.orphan)
	putEntry(t, f.orphan, "ab", hexName(1, "-a"), 10, 100*time.Hour)

	f.young = filepath.Join(scan, ".gocache-young")
	makeBuildCache(t, f.young)
	putEntry(t, f.young, "ab", hexName(1, "-a"), 10, time.Hour)

	f.live = filepath.Join(scan, ".gocache-live")
	makeBuildCache(t, f.live)
	putEntry(t, f.live, "ab", hexName(1, "-a"), 10, 100*time.Hour)

	f.held = filepath.Join(scan, "held-gocache")
	makeBuildCache(t, f.held)
	heldFile := putEntry(t, f.held, "ab", hexName(1, "-a"), 10, 100*time.Hour)

	r := goRunner(t, f.out)
	r.GoCacheRoot = f.shared
	r.GoModCacheRoot = filepath.Join(scan, "shared-mod")
	r.GoCacheScanRoots = []string{scan}
	r.ProcessEnv = func(context.Context) (string, error) {
		if procErr != nil {
			return "", procErr
		}
		return strings.ReplaceAll(procText, "LIVE", f.live), nil
	}
	paths := open
	r.OpenPaths = func(context.Context) (openfiles.Snapshot, error) {
		return openfiles.FromPaths(append(paths, heldFile)), nil
	}
	f.r = r
	return f
}

func TestOrphanedGoCachesReapsOnlyIdleUnreferenced(t *testing.T) {
	f := newOrphanFixture(t, "FOO=1 GOCACHE=LIVE/ go build\n", nil, nil)
	if err := f.r.OrphanedGoCaches(false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{f.orphan, f.young, f.live, f.held, f.shared} {
		if !exists(p) {
			t.Fatalf("dry-run removed %s", p)
		}
	}
	if !strings.Contains(f.out.String(), "would remove build cache "+f.orphan) {
		t.Fatalf("dry-run did not list the orphan:\n%s", f.out.String())
	}
	if err := f.r.OrphanedGoCaches(true); err != nil {
		t.Fatal(err)
	}
	if exists(f.orphan) {
		t.Fatal("orphan not reaped")
	}
	for _, p := range []string{f.young, f.live, f.held, f.shared} {
		if !exists(p) {
			t.Fatalf("wrongly removed %s", p)
		}
	}
}

func TestOrphanedGoCachesFailClosed(t *testing.T) {
	f := newOrphanFixture(t, "", nil, os.ErrPermission)
	if err := f.r.OrphanedGoCaches(true); err != nil {
		t.Fatal(err)
	}
	if !exists(f.orphan) || !strings.Contains(f.out.String(), "skipped") {
		t.Fatal("unreadable process listing must skip the layer")
	}
	f = newOrphanFixture(t, "x", nil, nil)
	f.r.OpenPaths = func(context.Context) (openfiles.Snapshot, error) { return openfiles.Snapshot{}, os.ErrPermission }
	if err := f.r.OrphanedGoCaches(true); err != nil {
		t.Fatal(err)
	}
	if !exists(f.orphan) {
		t.Fatal("failed lsof must skip the layer")
	}
}

func TestOrphanedGoCachesModCacheAndEmptyParent(t *testing.T) {
	f := newOrphanFixture(t, "x", nil, nil)
	holder := filepath.Join(f.scan, ".gocache-both")
	build := filepath.Join(holder, "build")
	makeBuildCache(t, build)
	putEntry(t, build, "ab", hexName(1, "-a"), 10, 100*time.Hour)
	mod := filepath.Join(holder, "mod")
	dl := filepath.Join(mod, "cache", "download", "example.com", "m", "@v")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	ro := filepath.Join(dl, "v1.zip")
	_ = os.WriteFile(ro, []byte("z"), 0o444)
	at := time.Now().Add(-100 * time.Hour)
	_ = os.Chtimes(ro, at, at)
	_ = os.Chmod(dl, 0o555)
	if err := f.r.OrphanedGoCaches(true); err != nil {
		t.Fatal(err)
	}
	if exists(holder) {
		t.Fatal("holder with only build+mod must be removed entirely")
	}
	if !exists(f.scan) || !exists(f.shared) {
		t.Fatal("scan root or shared cache removed")
	}
}

func TestOrphanedGoCachesNeverTouchesSharedAncestors(t *testing.T) {
	f := newOrphanFixture(t, "x", nil, nil)
	// A recognised cache that CONTAINS the shared cache must be left alone.
	outer := filepath.Join(f.scan, "outer")
	makeBuildCache(t, outer)
	putEntry(t, outer, "ab", hexName(1, "-a"), 10, 100*time.Hour)
	f.r.GoCacheRoot = filepath.Join(outer, "inner")
	makeBuildCache(t, f.r.GoCacheRoot)
	if err := f.r.OrphanedGoCaches(true); err != nil {
		t.Fatal(err)
	}
	if !exists(outer) || !exists(f.r.GoCacheRoot) {
		t.Fatal("cache containing the shared cache was removed")
	}
}

func TestOrphanedGoCachesRefusedUnderTestWhenUnset(t *testing.T) {
	var out strings.Builder
	r := goRunner(t, &out)
	if err := r.OrphanedGoCaches(true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "skip") {
		t.Fatalf("unset roots must be refused: %q", out.String())
	}
}

func TestOrphanedGoCachesStopsAtDeadline(t *testing.T) {
	f := newOrphanFixture(t, "x", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.r.Ctx = ctx
	_ = f.r.OrphanedGoCaches(true)
	if !exists(f.orphan) {
		t.Fatal("removed after the deadline")
	}
}
