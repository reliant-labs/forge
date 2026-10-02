package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTempSweep builds a sweep over a test-owned root with a faked lsof, so
// nothing in these tests depends on the real $TMPDIR or on what the machine
// happens to have open.
func newTempSweep(t *testing.T, root string, open map[string]bool, lsofErr error, out *strings.Builder) tempSweep {
	t.Helper()
	return tempSweep{
		root:   root,
		now:    time.Now(),
		maxAge: tempSweepAge,
		openPaths: func() (map[string]bool, error) {
			if lsofErr != nil {
				return nil, lsofErr
			}
			return open, nil
		},
		print: func(format string, args ...any) { fmt.Fprintf(out, format, args...) },
	}
}

// writeTempEntry creates root/name/file with the given mtime, and returns the
// entry's path. age is how long ago the content was last touched.
func writeTempEntry(t *testing.T, root, name string, age time.Duration, extra ...string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := append([]string{filepath.Join(dir, "scratch.o")}, extra...)
	for _, rel := range paths {
		p := rel
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, rel)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("scratch"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Set mtimes deepest-first: writing a child bumps its parent.
	stamp := time.Now().Add(-age)
	_ = filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			_ = os.Chtimes(p, stamp, stamp)
		}
		return nil
	})
	if err := os.Chtimes(dir, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestTempSweepCandidateSelection is the safety contract for the sweep, which
// runs against a directory shared with every other tool on the machine. Each
// case is a distinct reason to decline, and the one removal case proves the
// decline paths are not just a sweep that never fires.
func TestTempSweepCandidateSelection(t *testing.T) {
	old := 48 * time.Hour
	recent := 1 * time.Hour

	tests := []struct {
		name string
		// setup creates the entry and returns its path.
		setup      func(t *testing.T, root string) string
		open       func(path string) map[string]bool
		lsofErr    error
		wantRemove bool
		wantReason string
	}{
		{
			name:       "old allowlisted entry is removed",
			setup:      func(t *testing.T, root string) string { return writeTempEntry(t, root, "go-link-123", old) },
			wantRemove: true,
		},
		{
			name:       "old leaked forge test binary is removed",
			setup:      func(t *testing.T, root string) string { return writeTempEntry(t, root, "forge-e2e-bin-9", old) },
			wantRemove: true,
		},
		{
			name:       "old leaked kcleval forge binary is removed",
			setup:      func(t *testing.T, root string) string { return writeTempEntry(t, root, "forge-bin-9", old) },
			wantRemove: true,
		},
		{
			name:       "recently touched entry is kept",
			setup:      func(t *testing.T, root string) string { return writeTempEntry(t, root, "go-link-recent", recent) },
			wantReason: "recent",
		},
		{
			name: "old root with a recent file inside is kept",
			setup: func(t *testing.T, root string) string {
				dir := writeTempEntry(t, root, "go-link-mixed", old)
				fresh := filepath.Join(dir, "nested", "live.o")
				if err := os.MkdirAll(filepath.Dir(fresh), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fresh, []byte("live"), 0o600); err != nil {
					t.Fatal(err)
				}
				// Deliberately leave the ROOT's mtime old: the newest mtime
				// anywhere inside is what must decide.
				stamp := time.Now().Add(-old)
				if err := os.Chtimes(dir, stamp, stamp); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			wantReason: "recent",
		},
		{
			name:       "entry held open by a process is kept",
			setup:      func(t *testing.T, root string) string { return writeTempEntry(t, root, "go-build-open", old) },
			open:       func(path string) map[string]bool { return map[string]bool{filepath.Join(path, "scratch.o"): true} },
			wantReason: "in use",
		},
		{
			name:       "unknown prefix is never touched",
			setup:      func(t *testing.T, root string) string { return writeTempEntry(t, root, "someone-elses-data", old) },
			wantReason: "",
		},
		{
			name: "a .git FILE means a real worktree: kept",
			setup: func(t *testing.T, root string) string {
				return writeTempEntry(t, root, "forge-bin-worktree", old, ".git")
			},
			wantReason: "git metadata",
		},
		{
			name: "a .git DIR outside the fixture allowlist is kept",
			setup: func(t *testing.T, root string) string {
				dir := writeTempEntry(t, root, "forge-skill-validate-checkout", old, filepath.Join(".git", "HEAD"))
				return dir
			},
			wantReason: "git metadata",
		},
		{
			name: "a .git DIR under tierguard- is the fixture's own git init: removed",
			setup: func(t *testing.T, root string) string {
				return writeTempEntry(t, root, "tierguard-abc", old, filepath.Join(".git", "HEAD"))
			},
			wantRemove: true,
		},
		{
			name:       "lsof failure removes nothing",
			setup:      func(t *testing.T, root string) string { return writeTempEntry(t, root, "go-link-nolsof", old) },
			lsofErr:    fmt.Errorf("lsof not found"),
			wantReason: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := tc.setup(t, root)
			open := map[string]bool{}
			if tc.open != nil {
				open = tc.open(path)
			}
			var out strings.Builder
			s := newTempSweep(t, root, open, tc.lsofErr, &out)
			if err := s.run(true); err != nil {
				t.Fatalf("TempSweep: %v", err)
			}
			_, statErr := os.Stat(path)
			gone := os.IsNotExist(statErr)
			if gone != tc.wantRemove {
				t.Fatalf("entry removed = %v, want %v\nsweep output:\n%s", gone, tc.wantRemove, out.String())
			}
			if tc.wantReason != "" && !strings.Contains(out.String(), tc.wantReason) {
				t.Errorf("sweep did not report retaining the entry as %q:\n%s", tc.wantReason, out.String())
			}
		})
	}
}

// TestTempSweepDryRunRemovesNothing: the default is a report. A sweep that
// deleted on a plain `forge storage gc` would be a very expensive surprise.
func TestTempSweepDryRunRemovesNothing(t *testing.T) {
	root := t.TempDir()
	path := writeTempEntry(t, root, "go-link-dryrun", 48*time.Hour)
	var out strings.Builder
	s := newTempSweep(t, root, nil, nil, &out)
	if err := s.run(false); err != nil {
		t.Fatalf("TempSweep: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("dry run removed %s: %v", path, err)
	}
	if !strings.Contains(out.String(), "reclaimable") || !strings.Contains(out.String(), path) {
		t.Errorf("dry run did not report the candidate and its size:\n%s", out.String())
	}
}

// TestTempSweepRemovesReadOnlyTree pins the chmod pass. A leaked forge fixture
// contains a Go module cache (mode 0444 files in 0555 directories), so a plain
// RemoveAll fails with EACCES and the 3.9 GB of fixtures stay on disk.
func TestTempSweepRemovesReadOnlyTree(t *testing.T) {
	// writeModCacheTree builds the shape a leaked fixture has: a Go module
	// cache, whose files are 0444 inside 0555 directories.
	writeModCacheTree := func(parent, name string) (dir, modCache string) {
		dir = filepath.Join(parent, name)
		modCache = filepath.Join(dir, "pkg", "mod", "example.com", "dep@v1.0.0")
		if err := os.MkdirAll(modCache, 0o700); err != nil {
			t.Fatal(err)
		}
		locked := filepath.Join(modCache, "go.mod")
		if err := os.WriteFile(locked, []byte("module example.com/dep\n"), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(modCache, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(modCache, 0o700) // so t.TempDir cleanup can always succeed
		})
		// Age EVERY path in the tree: the sweep takes the newest mtime found
		// anywhere inside, so a single freshly-created intermediate directory
		// is enough to make the whole entry look active.
		stamp := time.Now().Add(-48 * time.Hour)
		_ = filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chtimes(p, stamp, stamp)
			}
			return nil
		})
		if err := os.Chtimes(dir, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return dir, modCache
	}

	// Assert the PREMISE on a throwaway copy rather than trusting the comment:
	// a plain RemoveAll must fail on this shape, which is why removeWritable
	// exists. It runs against its own tree because a partial RemoveAll bumps
	// the ancestors' mtimes, which would make the real candidate look recent.
	premise, _ := writeModCacheTree(t.TempDir(), "premise")
	if err := os.RemoveAll(premise); err == nil {
		t.Skip("this platform allows RemoveAll of a read-only tree; the chmod pass is untestable here")
	}

	root := t.TempDir()
	dir, _ := writeModCacheTree(root, "forge-e2e-bin-readonly")

	var out strings.Builder
	s := newTempSweep(t, root, nil, nil, &out)
	if err := s.run(true); err != nil {
		t.Fatalf("TempSweep: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("read-only fixture survived the sweep (%v):\n%s", err, out.String())
	}
}

// TestTempSweepRefusesTheRealTempDirUnderTest is the guard on the accident
// this layer would otherwise cause. Four pre-existing tests in this package
// call GC(ctx, true) to assert things about Docker endpoints and builder
// pruning (builders_test.go, storage_test.go), and GC runs the temp sweep —
// so without this refusal `go test ./internal/storage/` would apply-sweep the
// developer's real $TMPDIR. The identical hole in the Sources layer deleted
// five real source clones before it was closed.
//
// The refusal, not a convention, is the fix: a test asserting something about
// Docker cannot be expected to remember to scope a temp root.
func TestTempSweepRefusesTheRealTempDirUnderTest(t *testing.T) {
	var out strings.Builder
	r := Runner{Policy: DefaultPolicy(), Out: &out}
	// Skip, not fail: aborting here would break the unrelated GC assertions
	// that are the whole reason this path is reachable from a test.
	if err := r.TempSweep(true); err != nil {
		t.Fatalf("TempSweep must skip, not fail, so it never aborts an unrelated GC assertion: %v", err)
	}
	if !strings.Contains(out.String(), "TempRoot is unset under test") {
		t.Fatalf("the skip must name what to set:\n%s", out.String())
	}
	// Nothing was even considered: the real temp dir was never read.
	if strings.Contains(out.String(), "temp sweep: /") {
		t.Fatalf("an unscoped TempSweep planned real removals:\n%s", out.String())
	}
}

// TestGCDoesNotSweepTheRealTempDir pins the same guarantee through the caller
// that actually creates the hazard — GC itself, with apply=true, exactly as
// builders_test.go and storage_test.go invoke it. An old allowlisted entry
// sitting in an unscoped root must survive, because GC must never have been
// given a root it can delete from.
func TestGCDoesNotSweepTheRealTempDir(t *testing.T) {
	// Stand in for the real $TMPDIR: an entry that satisfies every sweep
	// condition (allowlisted prefix, 48h idle, not open, no git metadata).
	standIn := t.TempDir()
	victim := writeTempEntry(t, standIn, "go-link-victim", 48*time.Hour)

	var out strings.Builder
	// A Runner with NO TempRoot, as those four tests construct it.
	r := Runner{Policy: DefaultPolicy(), Out: &out}
	// GC fails later for want of a Docker daemon; the sweep runs before that,
	// so the error is irrelevant to what is being asserted here.
	_ = r.GC(context.Background(), true)

	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("GC(apply=true) with an unset TempRoot removed %s (%v) — a test "+
			"asserting something about Docker just deleted real temp data", victim, err)
	}
	if !strings.Contains(out.String(), "TempRoot is unset under test") {
		t.Errorf("GC did not report skipping the temp sweep:\n%s", out.String())
	}
}

// TestTempSweepHonorsAnExplicitRoot is the other half: the refusal must not be
// achievable by making the layer inert. Given a root, it still sweeps.
func TestTempSweepHonorsAnExplicitRoot(t *testing.T) {
	root := t.TempDir()
	doomed := writeTempEntry(t, root, "go-link-scoped", 48*time.Hour)

	var out strings.Builder
	r := Runner{Policy: DefaultPolicy(), Out: &out, TempRoot: root}
	if err := r.TempSweep(false); err != nil { // dry run
		t.Fatalf("TempSweep: %v", err)
	}
	if !strings.Contains(out.String(), doomed) {
		t.Fatalf("an explicitly-rooted sweep found no candidate — the refusal has "+
			"disabled the layer rather than scoping it:\n%s", out.String())
	}
	if _, err := os.Stat(doomed); err != nil {
		t.Errorf("dry run removed %s: %v", doomed, err)
	}
}

// writeGoTestTempRoot creates the exact shape `testing.T.TempDir` leaves when a
// test binary is killed before its Cleanup runs: root/<name>/<NNN>/... with
// every path aged by age.
func writeGoTestTempRoot(t *testing.T, root, name string, age time.Duration, children ...string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	for _, child := range children {
		p := filepath.Join(dir, child, "fixture", "main.go")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ageTree(t, dir, age)
	return dir
}

// ageTree sets every mtime under dir (and dir itself) to now-age. The sweep
// takes the NEWEST mtime anywhere inside an entry, so one fresh intermediate
// directory would make the whole entry look active.
func ageTree(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	stamp := time.Now().Add(-age)
	_ = filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			_ = os.Chtimes(p, stamp, stamp)
		}
		return nil
	})
	if err := os.Chtimes(dir, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

// TestTempSweepReclaimsOrphanedGoTestTempDirs pins the Go `t.TempDir()` rule.
// A test binary killed before its Cleanup runs (a CI timeout, an agent's
// interrupted `go test`, a SIGKILL from a supervisor) leaves its TempDir root
// behind forever. Measured on one Mac: five
// TestEveryDaemonPriorityClassRendersOnEveryDaemonCluster* roots held 22 GB,
// and 193 TestUpDeployResolveIdenticalPort* roots sat beside them.
//
// The rule is a fixed SHAPE, not a glob: Go names the root with os.MkdirTemp
// (test name, symbols stripped, then random digits) and names every child with
// %03d. Each case below is one way a directory can resemble that shape and
// still not be one, plus the existing safety model applied to the new shape.
func TestTempSweepReclaimsOrphanedGoTestTempDirs(t *testing.T) {
	old := 48 * time.Hour
	recent := 1 * time.Hour

	tests := []struct {
		name       string
		setup      func(t *testing.T, root string) string
		open       func(path string) map[string]bool
		wantRemove bool
		wantReason string
	}{
		{
			name: "idle Go test TempDir root is reclaimed",
			setup: func(t *testing.T, root string) string {
				return writeGoTestTempRoot(t, root, "TestEveryDaemonPriorityClassRendersOnEveryDaemonCluster2743100264", old, "001", "002")
			},
			wantRemove: true,
		},
		{
			name: "subtest root (slash stripped) with one child is reclaimed",
			setup: func(t *testing.T, root string) string {
				return writeGoTestTempRoot(t, root, "TestUpDeployResolveIdenticalPortsame_port1015013715", old, "001")
			},
			wantRemove: true,
		},
		{
			name: "recently touched Go test root is retained",
			setup: func(t *testing.T, root string) string {
				return writeGoTestTempRoot(t, root, "TestUpDeployResolveIdenticalPort1016300401", recent, "001")
			},
			wantReason: "recent",
		},
		{
			name: "Go test root held open by a running test is retained",
			setup: func(t *testing.T, root string) string {
				return writeGoTestTempRoot(t, root, "TestLongRunning123456", old, "001")
			},
			open: func(path string) map[string]bool {
				return map[string]bool{filepath.Join(path, "001", "fixture", "main.go"): true}
			},
			wantReason: "in use",
		},
		{
			name: "Go test root containing a git repository is retained",
			setup: func(t *testing.T, root string) string {
				dir := writeGoTestTempRoot(t, root, "TestScaffoldsARepo99", old, "001")
				if err := os.MkdirAll(filepath.Join(dir, "001", "app", ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
				ageTree(t, dir, old)
				return dir
			},
			wantReason: "git metadata",
		},
		{
			name: "Test-named dir with no numbered child is retained",
			setup: func(t *testing.T, root string) string {
				dir := writeGoTestTempRoot(t, root, "TestFoo123", old)
				if err := os.MkdirAll(filepath.Join(dir, "results"), 0o700); err != nil {
					t.Fatal(err)
				}
				ageTree(t, dir, old)
				return dir
			},
		},
		{
			name:  "empty Test-named dir is retained",
			setup: func(t *testing.T, root string) string { return writeGoTestTempRoot(t, root, "TestFoo456", old) },
		},
		{
			name: "numbered child beside a non-numbered one is retained",
			setup: func(t *testing.T, root string) string {
				dir := writeGoTestTempRoot(t, root, "TestMixed789", old, "001")
				if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o600); err != nil {
					t.Fatal(err)
				}
				ageTree(t, dir, old)
				return dir
			},
		},
		{
			name: "a numbered FILE is not Go's numbered directory: retained",
			setup: func(t *testing.T, root string) string {
				dir := writeGoTestTempRoot(t, root, "TestFiles321", old)
				if err := os.WriteFile(filepath.Join(dir, "001"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				ageTree(t, dir, old)
				return dir
			},
		},
		{
			name: "two-digit child is not Go's %03d: retained",
			setup: func(t *testing.T, root string) string {
				return writeGoTestTempRoot(t, root, "TestShort654", old, "01")
			},
		},
		{
			name: "Test name with no random suffix is retained",
			setup: func(t *testing.T, root string) string {
				return writeGoTestTempRoot(t, root, "TestFoo", old, "001")
			},
		},
		{
			name: "a user's own directory is never touched",
			setup: func(t *testing.T, root string) string {
				return writeGoTestTempRoot(t, root, "my-test-results-2026", old, "001")
			},
		},
		{
			// `go test` never runs Test<lower-case>, so this is not a test name.
			name: "Test followed by a lower-case letter is not a test: retained",
			setup: func(t *testing.T, root string) string {
				return writeGoTestTempRoot(t, root, "Testdata123", old, "001")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := tc.setup(t, root)
			open := map[string]bool{}
			if tc.open != nil {
				open = tc.open(path)
			}
			var out strings.Builder
			s := newTempSweep(t, root, open, nil, &out)
			if err := s.run(true); err != nil {
				t.Fatalf("TempSweep: %v", err)
			}
			_, statErr := os.Stat(path)
			gone := os.IsNotExist(statErr)
			if gone != tc.wantRemove {
				t.Fatalf("entry removed = %v, want %v\nsweep output:\n%s", gone, tc.wantRemove, out.String())
			}
			if tc.wantReason != "" && !strings.Contains(out.String(), tc.wantReason) {
				t.Errorf("sweep did not report retaining the entry as %q:\n%s", tc.wantReason, out.String())
			}
		})
	}
}

// TestGoTestTempDirNameMatchesWhatGoCreates derives the root name from the real
// testing package rather than trusting a hand-written example: if Go ever
// changes how TempDir names its root, the sweep's shape must follow, and this
// is where that shows up.
func TestGoTestTempDirNameMatchesWhatGoCreates(t *testing.T) {
	for _, sub := range []string{"plain", "with spaces/and slashes", strings.Repeat("long", 30)} {
		t.Run(sub, func(t *testing.T) {
			dir := t.TempDir() // <tmp>/<root>/001
			root := filepath.Base(filepath.Dir(dir))
			if !goTestTempRootName(root) {
				t.Fatalf("goTestTempRootName(%q) = false for a root Go itself created", root)
			}
			if !goTestTempChildName(filepath.Base(dir)) {
				t.Fatalf("goTestTempChildName(%q) = false for a child Go itself created", filepath.Base(dir))
			}
		})
	}
}

// TestTempSweepPrefixAllowlistIsClosed documents the allowlist as a decision
// rather than an accident: a name that is merely temp-looking is not swept.
func TestTempSweepPrefixAllowlistIsClosed(t *testing.T) {
	allowed := []string{"go-link-abc", "go-build123", "embedded_postgres_log42",
		"forge-e2e-bin-1", "forge-bin-1", "forge-skill-validate-1",
		"forge-kcl-module-test-1", "tierguard-1", "forge-k3d-1"}
	for _, name := range allowed {
		if !tempSweepAllowed(name) {
			t.Errorf("tempSweepAllowed(%q) = false, want true", name)
		}
	}
	denied := []string{"", "node-compile-cache", "com.apple.launchd.x", "forge", "pytest-of-user",
		"go", "golink-abc", "tierguard", "TemporaryItems", "my-forge-e2e-bin-1"}
	for _, name := range denied {
		if tempSweepAllowed(name) {
			t.Errorf("tempSweepAllowed(%q) = true, want false — the allowlist must stay closed", name)
		}
	}
}
