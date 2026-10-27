package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/forgedelegate"
)

func safeVersionDir(v string) string { return v }

// pinSelfExecutable makes this process "be" the fake go-run forge exe, with
// the durable copies filed under cache, and clears the memoized answer.
func pinSelfExecutable(t *testing.T, exe, version, cache string) {
	t.Helper()
	prevSrc, prevVersion := selfExecutableSource, selfVersion
	t.Cleanup(func() {
		selfExecutableSource, selfVersion = prevSrc, prevVersion
		resetSelfExecutable()
	})
	selfExecutableSource = func() (string, error) { return exe, nil }
	selfVersion = func() string { return version }
	t.Setenv(forgedelegate.CacheDirEnv, cache)
	resetSelfExecutable()
}

const selfTestVersion = "v0.1.46-0.20261014193438-7410e351b23b"

// removeGoRunBuild deletes the build the fake forge was started from, the way
// a trim or `go clean -cache` deletes <GOCACHE>/xx/<hash>-d.
// fakeRunnableForge is fakeGoRunForge whose executable says what it is.
func fakeRunnableForge(t *testing.T) string {
	t.Helper()
	exe, _ := fakeGoRunForge(t, "7410e351b23b")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho \"the pinned forge $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func removeGoRunBuild(t *testing.T, exe string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Dir(exe)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(exe); !os.IsNotExist(err) {
		t.Fatalf("the original executable is still there: %v", err)
	}
}

// forge started by `go run` lives in the build cache. Once the cache entry is
// gone (a trim, `go clean -cache`) the plugin path buf is handed and the
// re-exec path must still name a runnable forge.
func TestSelfExecutable_SurvivesTheGoRunBuildBeingDeleted(t *testing.T) {
	exe := fakeRunnableForge(t)
	cache := t.TempDir()
	pinSelfExecutable(t, exe, selfTestVersion, cache)

	// Startup: the copy is made before anything can wait for a heavy slot.
	startup, err := selfExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if startup == exe {
		t.Fatalf("a go-run forge must not refer to itself at %s", exe)
	}

	removeGoRunBuild(t, exe)

	parts, err := forgeExecCommand()
	if err != nil {
		t.Fatalf("forgeExecCommand: %v", err)
	}
	if parts[0] == exe {
		t.Fatalf("plugin/re-exec path %s is the deleted original", parts[0])
	}
	if !strings.HasPrefix(parts[0], cache) {
		t.Errorf("self-reference %s is outside the durable cache %s", parts[0], cache)
	}
	// buf runs [exe, protoc-gen-forge]; a re-exec runs the same path.
	out, err := exec.Command(parts[0], "protoc-gen-forge").CombinedOutput()
	if err != nil {
		t.Fatalf("running the self-reference after the original was deleted: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "the pinned forge") {
		t.Errorf("ran something else: %q", out)
	}
}

// The generate fingerprint hashes the forge executable by path: after the
// original is gone that stat used to fail ("running every step").
func TestSelfExecutable_GenerateFingerprintStillReadsTheForge(t *testing.T) {
	exe, _ := fakeGoRunForge(t, "7410e351b23b")
	cache := t.TempDir()
	pinSelfExecutable(t, exe, selfTestVersion, cache)
	if _, err := selfExecutable(); err != nil {
		t.Fatal(err)
	}
	removeGoRunBuild(t, exe)

	path, err := osGenerateCacheSys().executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the fingerprint's forge executable %s is unreadable: %v", path, err)
	}
	if _, err := (&execIdentities{prev: nil, next: map[string]execStamp{}, now: osGenerateCacheSys().now()}).identify(path, false); err != nil {
		t.Fatalf("fingerprint the forge executable: %v", err)
	}
}

// The copy is made once per identity and shared by every later start; a
// second process of the same forge does not rewrite it.
func TestSelfExecutable_CopyIsOncePerIdentity(t *testing.T) {
	exe, _ := fakeGoRunForge(t, "7410e351b23b")
	cache := t.TempDir()
	pinSelfExecutable(t, exe, selfTestVersion, cache)
	first, err := selfExecutable()
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(first)

	resetSelfExecutable()
	second, err := selfExecutable()
	if err != nil || second != first {
		t.Fatalf("second start = %q, %v; want the same copy %q", second, err, first)
	}
	after, _ := os.Stat(second)
	if !os.SameFile(before, after) {
		t.Error("the second start replaced the copy")
	}
	if entries, _ := os.ReadDir(filepath.Join(cache, safeVersionDir(selfTestVersion), runtime.GOOS+"-"+runtime.GOARCH)); len(entries) != 1 {
		t.Errorf("leftover temp files beside the copy: %v", entries)
	}
}

// A forge installed at a real path is already durable and names itself.
func TestSelfExecutable_InstalledForgeNamesItself(t *testing.T) {
	_, _ = fakeGoRunForge(t, "7410e351b23b")
	cache := t.TempDir()
	installed := "/opt/forge/bin/forge"
	pinSelfExecutable(t, installed, selfTestVersion, cache)
	got, err := selfExecutable()
	if err != nil || got != installed {
		t.Fatalf("selfExecutable = %q, %v; want %q", got, err, installed)
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Errorf("an installed forge copied itself: %v", entries)
	}
}

// The test binary is re-executed as the plugin by TestMain; it must keep
// naming itself, and a forge with no identity to file a copy under (a dev
// build) keeps today's behaviour rather than failing.
func TestSelfExecutable_FallsBackToTheExecutableWhenNoCopyCanBeFiled(t *testing.T) {
	exe, _ := fakeGoRunForge(t, "")
	cache := t.TempDir()
	pinSelfExecutable(t, exe, "dev", cache)
	got, err := selfExecutable()
	if err != nil || got != exe {
		t.Fatalf("selfExecutable = %q, %v; want the executable itself %q", got, err, exe)
	}

	self, _ := os.Executable()
	pinSelfExecutable(t, self, selfTestVersion, cache)
	if got, err := selfExecutable(); err != nil || got != self {
		t.Fatalf("test binary: selfExecutable = %q, %v; want %q", got, err, self)
	}
}
