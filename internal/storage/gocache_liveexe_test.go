package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// putExecEntry makes an executable entry (<root>/<shard>/<64 hex>-d/<name>) of
// the given age and returns its directory and binary.
func putExecEntry(t *testing.T, root, shard string, n int, age time.Duration) (dir, bin string) {
	t.Helper()
	dir = filepath.Join(root, shard, hexName(n, "-d"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "forge")
	if err := os.WriteFile(bin, make([]byte, 100), 0o755); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(dir, at, at); err != nil {
		t.Fatal(err)
	}
	return dir, bin
}

type execCacheFixture struct {
	r                      Runner
	root, liveDir, idleDir string
	out                    *strings.Builder
}

// execCache is a cache where everything is long past the floor, plus two
// executable entries: one a live process runs, one nothing runs.
func execCache(t *testing.T, running func(root string) []LiveExecutable, listErr error) execCacheFixture {
	t.Helper()
	root := t.TempDir()
	makeBuildCache(t, root)
	putEntry(t, root, "aa", hexName(1, "-a"), 100, 100*time.Hour)
	liveDir, liveBin := putExecEntry(t, root, "bb", 2, 100*time.Hour)
	idleDir, _ := putExecEntry(t, root, "cc", 3, 100*time.Hour)
	out := &strings.Builder{}
	r := goRunner(t, out)
	r.GoCacheRoot = root
	r.Policy.DeleteRatePerSec = 1_000_000_000
	r.Policy.MaxDeletesPerPass = 1_000_000
	r.LiveExecutables = func(_ context.Context, root string) ([]LiveExecutable, error) {
		if listErr != nil {
			return nil, listErr
		}
		return running(liveBin), nil
	}
	return execCacheFixture{r: r, root: root, liveDir: liveDir, idleDir: idleDir, out: out}
}

// An old executable entry that a live process runs survives every age and
// size rule; the idle one beside it, and ordinary entries, go.
func TestGoCacheTrimNeverRemovesALiveExecutableWhateverItsAge(t *testing.T) {
	f := execCache(t, func(bin string) []LiveExecutable {
		// Older than goCacheLongLived: the age lease alone would ignore it.
		return []LiveExecutable{{Path: bin, Age: 40 * time.Hour}}
	}, nil)
	r, root, liveDir, idleDir, out := f.r, f.root, f.liveDir, f.idleDir, f.out
	if err := r.SharedGoCache(true); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !exists(liveDir) {
		t.Fatalf("the executable entry of a running process was removed:\n%s", out)
	}
	if exists(idleDir) {
		t.Errorf("an idle executable entry was kept:\n%s", out)
	}
	if got := countEntries(root); got != 1 {
		t.Errorf("%d entries left, want only the live executable entry\n%s", got, out)
	}
	if !strings.Contains(out.String(), "1 executable entries of running processes kept") {
		t.Errorf("the pass does not say it kept a live executable:\n%s", out)
	}
}

// A process whose binary was unlinked from under it ("(deleted)") is not a
// reason to keep anything, but a still-present one is, and the suffix does not
// hide it from the match.
func TestCacheEntryOfExecutable(t *testing.T) {
	root := "/var/cache/workspace/go-build"
	hash := strings.Repeat("0a", 32)
	for exe, want := range map[string]string{
		root + "/9f/" + hash + "-d/forge":                        "9f/" + hash + "-d",
		root + "/9f/" + hash + "-d/forge (deleted)":              "9f/" + hash + "-d",
		root + "/9f/" + hash + "-a":                              "",
		root + "/9f/" + hash + "-d":                              "",
		root + "/README":                                         "",
		"/usr/local/bin/forge":                                   "",
		"/var/cache/workspace/go-build2/9f/" + hash + "-d/forge": "",
	} {
		got, ok := cacheEntryOfExecutable(root, exe)
		if got != want || ok != (want != "") {
			t.Errorf("cacheEntryOfExecutable(%q) = %q, %v; want %q", exe, got, ok, want)
		}
	}
}

// Fail closed: when /proc cannot be read, every -d entry is kept, however old,
// while ordinary entries are still trimmed.
func TestGoCacheTrimKeepsEveryExecutableEntryWhenProcIsUnreadable(t *testing.T) {
	f := execCache(t, nil, errors.New("open /proc: permission denied"))
	r, root, liveDir, idleDir, out := f.r, f.root, f.liveDir, f.idleDir, f.out
	if err := r.SharedGoCache(true); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !exists(liveDir) || !exists(idleDir) {
		t.Fatalf("live=%v idle=%v: with /proc unreadable no -d entry may be removed\n%s", exists(liveDir), exists(idleDir), out)
	}
	if got := countEntries(root); got != 2 {
		t.Errorf("%d entries left, want the 2 executable entries (the -a entry is still trimmed)\n%s", got, out)
	}
	for _, want := range []string{"cannot list running executables", "keeping every executable entry"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// The control: with nothing running, both executable entries go. Proves the
// guard, not the floor, kept them.
func TestGoCacheTrimWithNoLiveExecutableRemovesOldExecutableEntries(t *testing.T) {
	f := execCache(t, func(string) []LiveExecutable { return nil }, nil)
	r, root, out := f.r, f.root, f.out
	if err := r.SharedGoCache(true); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := countEntries(root); got != 0 {
		t.Errorf("%d entries left, want 0\n%s", got, out)
	}
}

// Dry run reports the same decision without removing anything.
func TestGoCacheDryRunListsNoLiveExecutable(t *testing.T) {
	f := execCache(t, func(bin string) []LiveExecutable {
		return []LiveExecutable{{Path: bin, Age: time.Hour}}
	}, nil)
	r, liveDir, idleDir, out := f.r, f.liveDir, f.idleDir, f.out
	if err := r.SharedGoCache(false); err != nil {
		t.Fatal(err)
	}
	if !exists(liveDir) || !exists(idleDir) {
		t.Fatal("a dry run removed something")
	}
	if strings.Contains(out.String(), liveDir) {
		t.Errorf("the dry run lists the live executable entry as removable:\n%s", out)
	}
	if !strings.Contains(out.String(), idleDir) {
		t.Errorf("the dry run does not list the idle executable entry:\n%s", out)
	}
}

// #1243's lease also counts a process whose executable lives in the cache:
// `go run`'s child is `forge`, which is not a toolchain command. A 2.5h-old
// such process holds entries a floor-only trim would remove.
func TestGoCacheLeaseCountsProcessesRunningFromTheCache(t *testing.T) {
	r, root, out := overFloorCache(t, ageOf()) // no toolchain process at all
	liveDir, liveBin := putExecEntry(t, root, "ee", 9, 150*time.Minute)
	r.LiveExecutables = func(context.Context, string) ([]LiveExecutable, error) {
		return []LiveExecutable{{Path: liveBin, Age: 150 * time.Minute}}, nil
	}
	_ = r.SharedGoCache(true)
	// The running process may hold anything last used after its start minus
	// the hour: the 3h-old entries stay, the 5h-old ones go.
	if got := countEntries(root); got != 16*20+1 {
		t.Fatalf("%d entries left, want the %d that are 3h old plus the live executable\n%s", got, 16*20+1, out)
	}
	if !exists(liveDir) {
		t.Fatal("live executable entry removed")
	}
	if !strings.Contains(out.String(), "held back by a running Go build 2h30m0s old") {
		t.Fatalf("the pass does not say a running process held it back:\n%s", out)
	}
}

// ... and the control: without that process the same cache trims past the
// floor, 3h-old entries included.
func TestGoCacheLeaseWithoutAProcessRunningFromTheCacheTrimsPastTheFloor(t *testing.T) {
	r, root, _ := overFloorCache(t, ageOf())
	r.LiveExecutables = func(context.Context, string) ([]LiveExecutable, error) { return nil, nil }
	_ = r.SharedGoCache(true)
	if got := countEntries(root); got != 0 {
		t.Fatalf("%d entries left, want 0", got)
	}
}
