package storage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/openfiles"
)

func unit(key string, class modClass, dir, zip int64, age time.Duration) *modUnit {
	return &modUnit{key: key, class: class, dirBytes: dir, zipBytes: zip, lastUse: time.Now().Add(-age)}
}

func TestPlanModEvictionOrdersTiersAndNeverTouchesPinned(t *testing.T) {
	units := []*modUnit{
		unit("pinned@v1", modPinned, 100, 10, 90*24*time.Hour),
		unit("idle-old@v1", modIdleRef, 50, 5, 40*24*time.Hour),
		unit("free-new@v1", modFree, 30, 3, 2*24*time.Hour),
		unit("free-old@v1", modFree, 40, 4, 30*24*time.Hour),
	}
	// 242 total. Target 172: free trees (40+30) are enough, no zip goes.
	actions, total, pinned, left := planModEviction(units, 172)
	if total != 242 || pinned != 110 {
		t.Fatalf("total %d pinned %d", total, pinned)
	}
	if len(actions) != 2 || actions[0].unit.key != "free-old@v1" || actions[1].unit.key != "free-new@v1" || actions[0].full || actions[1].full {
		t.Fatalf("actions = %+v", actions)
	}
	if left != 172 {
		t.Fatalf("left = %d", left)
	}

	// Target 100: free trees, then free zips, then the idle-referenced version.
	actions, _, _, left = planModEviction(units, 100)
	var got []string
	for _, a := range actions {
		got = append(got, a.unit.key+map[bool]string{true: "+zip", false: ""}[a.full])
	}
	want := "free-old@v1 free-new@v1 free-old@v1+zip free-new@v1+zip idle-old@v1 idle-old@v1+zip"
	if strings.Join(got, " ") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
	if left != 110 {
		t.Fatalf("pinned bytes must remain: left = %d", left)
	}
	for _, a := range actions {
		if a.unit.class == modPinned {
			t.Fatal("a pinned version was planned")
		}
	}
}

func TestPlanModEvictionWithinBudgetPlansNothing(t *testing.T) {
	actions, _, _, _ := planModEviction([]*modUnit{unit("a@v1", modFree, 10, 1, time.Hour)}, 100)
	if len(actions) != 0 {
		t.Fatalf("%+v", actions)
	}
}

func TestGoSumZipKeysSkipsGoModLinesAndEscapesPaths(t *testing.T) {
	p := filepath.Join(t.TempDir(), "go.sum")
	body := "github.com/Foo/bar v1.0.0 h1:aaa=\ngithub.com/Foo/bar v1.0.0/go.mod h1:bbb=\ngolang.org/x/mod v0.1.0 h1:ccc=\nbroken line\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := goSumZipKeys(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "github.com/!foo/bar@v1.0.0" || keys[1] != "golang.org/x/mod@v0.1.0" {
		t.Fatalf("keys = %v", keys)
	}
}

// modFixture is a module cache of three versions plus one checkout whose go.sum
// names one of them.
type modFixture struct {
	r        Runner
	root     string
	out      *strings.Builder
	checkout string
}

func (f modFixture) dir(key string) string { return filepath.Join(f.root, filepath.FromSlash(key)) }
func (f modFixture) zip(path, ver string) string {
	return filepath.Join(f.root, "cache", "download", filepath.FromSlash(path), "@v", ver+".zip")
}

func newModFixture(t *testing.T) modFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	root := filepath.Join(t.TempDir(), "mod")
	checkout := filepath.Join(t.TempDir(), "proj")
	for _, d := range []string{filepath.Join(root, "cache", "download"), checkout} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	add := func(path, ver string, size int, mtime time.Time) {
		tree := filepath.Join(root, filepath.FromSlash(path)+"@"+ver)
		if err := os.MkdirAll(tree, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(tree, "a.go"), make([]byte, size), 0o444)
		_ = os.Chmod(tree, 0o555) // cmd/go writes the tree read-only
		v := filepath.Join(root, "cache", "download", filepath.FromSlash(path), "@v")
		_ = os.MkdirAll(v, 0o755)
		for ext, n := range map[string]int{".zip": size / 10, ".ziphash": 5, ".info": 2, ".mod": 2} {
			p := filepath.Join(v, ver+ext)
			_ = os.WriteFile(p, make([]byte, n), 0o644)
			_ = os.Chtimes(p, mtime, mtime)
		}
		_ = os.Chtimes(tree, mtime, mtime)
	}
	add("example.com/kept", "v1.0.0", 4000, old)         // named by the checkout's go.sum
	add("example.com/stale", "v1.0.0", 4000, old)        // referenced by nobody
	add("example.com/fresh", "v1.0.0", 4000, time.Now()) // fetched just now
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "i"}} {
		if out, err := exec.Command("git", append([]string{"-C", checkout}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	sum := filepath.Join(checkout, "go.sum")
	_ = os.WriteFile(sum, []byte("example.com/kept v1.0.0 h1:x=\nexample.com/kept v1.0.0/go.mod h1:y=\n"), 0o644)
	_ = os.Chtimes(sum, old, old)
	if out, err := exec.Command("git", "-C", checkout, "add", "go.sum").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	_ = os.Chtimes(filepath.Join(checkout, "go.mod"), old, old)
	p := DefaultPolicy()
	p.GoModCacheGiB = 1
	p.Projects = []string{checkout}
	out := &strings.Builder{}
	r := Runner{
		Policy: p, Out: out, GoCacheRoot: t.TempDir(), GoModCacheRoot: root,
		OpenPaths: func(context.Context) (openfiles.Snapshot, error) {
			return openfiles.FromPaths([]string{"/nonexistent/x"}), nil
		},
		ProcessEnv: func(context.Context) (string, error) { return "go build ./...", nil },
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, _ error) error { _ = os.Chmod(p, 0o755); return nil })
	})
	return modFixture{r: r, root: root, out: out, checkout: checkout}
}

// shrink sets the budget in bytes by way of the volume cap: the policy is in
// whole GiB, so the test lowers the cap with a fake volume size.
func shrinkBudget(t *testing.T, bytes uint64) {
	t.Helper()
	orig := goCacheDiskSpace
	goCacheDiskSpace = func(string) (Disk, error) { return Disk{Capacity: bytes * 100 / defaultGoCacheVolumePercent}, nil }
	t.Cleanup(func() { goCacheDiskSpace = orig })
}

func pathExists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestGoModCacheEvictsOnlyWhatNothingReferences(t *testing.T) {
	f := newModFixture(t)
	shrinkBudget(t, 11200) // 3 versions of 4405 B = 13.2 KB; evicting the unreferenced one reaches the 8.9 KB low-water
	err := f.r.GoModCache(true)
	var unreachable *BudgetUnreachableError
	if err != nil && !strings.Contains(err.Error(), "over its budget") {
		t.Fatalf("GoModCache: %v\n%s", err, f.out)
	}
	_ = unreachable
	if pathExists(f.dir("example.com/stale@v1.0.0")) || pathExists(f.zip("example.com/stale", "v1.0.0")) {
		t.Errorf("the unreferenced version survived:\n%s", f.out)
	}
	if !pathExists(f.dir("example.com/fresh@v1.0.0")) || !pathExists(f.zip("example.com/fresh", "v1.0.0")) {
		t.Error("a version fetched just now was evicted")
	}
	// The checkout is idle (go.sum is 10 days old), so its version goes only
	// after the unreferenced one, and only if the budget still demands it.
	if !pathExists(f.dir("example.com/kept@v1.0.0")) {
		t.Errorf("a referenced version was evicted while an unreferenced one could go:\n%s", f.out)
	}
	if pathExists(filepath.Join(f.root, "cache", modCacheAsideDir)) {
		entries, _ := os.ReadDir(filepath.Join(f.root, "cache", modCacheAsideDir))
		if len(entries) != 0 {
			t.Errorf("trees left aside: %v", entries)
		}
	}
	// .info and .mod always stay: they keep offline graph loading working.
	for _, ext := range []string{".info", ".mod"} {
		if !pathExists(strings.TrimSuffix(f.zip("example.com/stale", "v1.0.0"), ".zip") + ext) {
			t.Errorf("stale%s was removed", ext)
		}
	}
}

func TestGoModCacheKeepsAVersionAnActiveCheckoutNames(t *testing.T) {
	f := newModFixture(t)
	now := time.Now()
	_ = os.Chtimes(filepath.Join(f.checkout, "go.sum"), now, now) // edited just now: active
	shrinkBudget(t, 100)                                          // far under: everything evictable goes
	_ = f.r.GoModCache(true)
	if !pathExists(f.dir("example.com/kept@v1.0.0")) || !pathExists(f.zip("example.com/kept", "v1.0.0")) {
		t.Fatalf("a version an active checkout references was evicted:\n%s", f.out)
	}
	if !pathExists(f.dir("example.com/fresh@v1.0.0")) {
		t.Fatal("the fresh version was evicted")
	}
	if pathExists(f.dir("example.com/stale@v1.0.0")) {
		t.Fatal("the unreferenced version survived")
	}
	if !strings.Contains(f.out.String(), "WARNING") {
		t.Errorf("a budget that pinned bytes keep over it must be reported:\n%s", f.out)
	}
}

func TestGoModCacheKeepsAVersionAProcessNamesOrHolds(t *testing.T) {
	f := newModFixture(t)
	f.r.ProcessEnv = func(context.Context) (string, error) {
		return "go run example.com/stale@v1.0.0", nil
	}
	shrinkBudget(t, 100)
	_ = f.r.GoModCache(true)
	if !pathExists(f.dir("example.com/stale@v1.0.0")) {
		t.Fatalf("a version named on a live command line was evicted:\n%s", f.out)
	}

	f = newModFixture(t)
	held := f.dir("example.com/stale@v1.0.0")
	f.r.OpenPaths = func(context.Context) (openfiles.Snapshot, error) {
		return openfiles.FromPaths([]string{filepath.Join(held, "a.go")}), nil
	}
	shrinkBudget(t, 100)
	_ = f.r.GoModCache(true)
	if !pathExists(held) {
		t.Fatalf("a version with an open file was evicted:\n%s", f.out)
	}
}

func TestGoModCacheKeepsAVersionBeingDownloaded(t *testing.T) {
	f := newModFixture(t)
	unlock, err := tryLockFile(strings.TrimSuffix(f.zip("example.com/stale", "v1.0.0"), ".zip") + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	shrinkBudget(t, 100)
	_ = f.r.GoModCache(true)
	if !pathExists(f.dir("example.com/stale@v1.0.0")) {
		t.Fatalf("a version whose cmd/go lock is held was evicted:\n%s", f.out)
	}
}

func TestGoModCacheDryRunRemovesNothingAndNoReferencesRefuses(t *testing.T) {
	f := newModFixture(t)
	shrinkBudget(t, 100)
	if err := f.r.GoModCache(false); err != nil && !strings.Contains(err.Error(), "over its budget") {
		t.Fatal(err)
	}
	if !pathExists(f.dir("example.com/stale@v1.0.0")) || !strings.Contains(f.out.String(), "would remove") {
		t.Fatalf("a dry run must report and remove nothing:\n%s", f.out)
	}

	f = newModFixture(t)
	f.r.Policy.Projects = nil // no checkout: which versions are in use is unknown
	shrinkBudget(t, 100)
	if err := f.r.GoModCache(true); err != nil {
		t.Fatal(err)
	}
	if !pathExists(f.dir("example.com/stale@v1.0.0")) || !strings.Contains(f.out.String(), "cannot determine") {
		t.Fatalf("with no references known nothing may be evicted:\n%s", f.out)
	}
}

func TestGoModCacheFinishesInterruptedEvictions(t *testing.T) {
	f := newModFixture(t)
	aside := filepath.Join(f.root, "cache", modCacheAsideDir, "1-1")
	if err := os.MkdirAll(filepath.Join(aside, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(aside, "sub", "x"), []byte("x"), 0o444)
	_ = os.Chmod(filepath.Join(aside, "sub"), 0o555)
	if err := f.r.GoModCache(true); err != nil {
		t.Fatal(err)
	}
	if pathExists(aside) {
		t.Fatal("a tree an earlier pass renamed aside was not finished")
	}
}

func TestGoModCacheWithinBudgetDoesNotGatherEvidence(t *testing.T) {
	f := newModFixture(t)
	f.r.ProcessEnv = nil // unset under test would error if it were consulted
	if err := f.r.GoModCache(true); err != nil {
		t.Fatalf("within budget it must not need the process list: %v\n%s", err, f.out)
	}
	if !strings.Contains(f.out.String(), "within its") {
		t.Fatalf("%s", f.out)
	}
}

func TestGoModCacheDryRunCountsATreeOnce(t *testing.T) {
	f := newModFixture(t)
	shrinkBudget(t, 100) // everything evictable goes: tree, then zip, of the stale and the idle-referenced version
	var trim GoCacheTrim
	f.r.OnGoCacheTrim = func(g GoCacheTrim) { trim = g }
	_ = f.r.GoModCache(false)
	// Two evictable versions of 4000 B tree + 405 B zip: 8810 B at most.
	if trim.FreedBytes == 0 || trim.FreedBytes > 8810 {
		t.Fatalf("dry-run freed %d bytes, want at most 8810 (a tree counted once)", trim.FreedBytes)
	}
}
