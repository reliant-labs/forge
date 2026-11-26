package toolcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const tool = "fake-tool"

// writeTool is a build that writes the binary slowly, a byte at a time, so a
// reader of a half-built entry would see a short file.
func writeTool(content string) func(context.Context, string) error {
	return func(_ context.Context, stage string) error {
		f, err := os.Create(filepath.Join(stage, tool))
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		for i := range content {
			if _, err := f.WriteString(content[i : i+1]); err != nil {
				return err
			}
			time.Sleep(time.Millisecond)
		}
		return nil
	}
}

func readyWith(content string) func(string) bool {
	return func(dir string) bool {
		b, err := os.ReadFile(filepath.Join(dir, tool))
		return err == nil && string(b) == content
	}
}

// Many forge processes miss the same key at once (every fresh project's first
// generate pins protoc-gen-go). Each may build; every one of them gets a
// complete entry, and no staging directory is left behind.
func TestEnsure_ConcurrentMissesAllGetACompleteEntry(t *testing.T) {
	t.Parallel()
	kindDir := t.TempDir()
	want := strings.Repeat("x", 32)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir, err := Ensure(context.Background(), kindDir, "fake@v1", readyWith(want), writeTool(want))
			if err == nil && !readyWith(want)(dir) {
				err = fmt.Errorf("Ensure returned %s, which is not a complete entry", dir)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertNoStaging(t, kindDir)
}

// A reader never sees a half-written binary: the entry appears by one rename.
func TestEnsure_ReadersNeverSeeAPartialEntry(t *testing.T) {
	t.Parallel()
	kindDir := t.TempDir()
	want := strings.Repeat("y", 64)
	bin := filepath.Join(kindDir, "fake@v1", tool)
	done := make(chan error, 1)
	go func() {
		_, err := Ensure(context.Background(), kindDir, "fake@v1", readyWith(want), writeTool(want))
		done <- err
	}()
	for running := true; running; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			running = false
		default:
		}
		if b, err := os.ReadFile(bin); err == nil && string(b) != want {
			t.Fatalf("a reader saw a partial binary (%d of %d bytes)", len(b), len(want))
		}
	}
}

func TestEnsure_FailedBuildLeavesNothingAndIsRetried(t *testing.T) {
	t.Parallel()
	kindDir := t.TempDir()
	want := "ok"
	failing := func(_ context.Context, stage string) error {
		_ = os.WriteFile(filepath.Join(stage, tool), []byte("half"), 0o755)
		return errors.New("network is unreachable")
	}
	if _, err := Ensure(context.Background(), kindDir, "fake@v1", readyWith(want), failing); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v; want the build's error", err)
	}
	if _, err := os.Stat(filepath.Join(kindDir, "fake@v1")); !os.IsNotExist(err) {
		t.Errorf("a failed build published an entry: %v", err)
	}
	assertNoStaging(t, kindDir)
	if _, err := Ensure(context.Background(), kindDir, "fake@v1", readyWith(want), writeTool(want)); err != nil {
		t.Fatalf("the retry must build: %v", err)
	}
}

// An entry ready rejects (a wrong or corrupt build, or one an older forge
// wrote into in place) is replaced whole, never built into.
func TestEnsure_ReplacesAnUnusableEntry(t *testing.T) {
	t.Parallel()
	kindDir := t.TempDir()
	dir := filepath.Join(kindDir, "fake@v1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tool), []byte("wrong"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), kindDir, "fake@v1", readyWith("right"), writeTool("right")); err != nil {
		t.Fatal(err)
	}
	if !readyWith("right")(dir) {
		t.Error("the unusable entry was not replaced")
	}
	if _, err := os.Stat(filepath.Join(dir, "stray")); !os.IsNotExist(err) {
		t.Error("the replacement kept a file of the old entry: it was built into, not replaced")
	}
	if left, _ := filepath.Glob(filepath.Join(kindDir, replacedPrefix+"*")); len(left) != 0 {
		t.Errorf("the replaced entry was left behind: %v", left)
	}
}

// A ready entry is used as is, never rebuilt.
func TestEnsure_HitDoesNotBuild(t *testing.T) {
	t.Parallel()
	kindDir := t.TempDir()
	if _, err := Ensure(context.Background(), kindDir, "fake@v1", readyWith("v"), writeTool("v")); err != nil {
		t.Fatal(err)
	}
	built := false
	if _, err := Ensure(context.Background(), kindDir, "fake@v1", readyWith("v"), func(context.Context, string) error { built = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if built {
		t.Error("a ready entry was rebuilt")
	}
}

func TestEnsure_RejectsKeysThatAreNotOneName(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"", ".hidden", "a/b", `a\b`, ".."} {
		if _, err := Ensure(context.Background(), t.TempDir(), key, readyWith("v"), writeTool("v")); err == nil {
			t.Errorf("key %q was accepted", key)
		}
	}
}

func TestRoot_HonoursFORGE_TOOLS_CACHE(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvRoot, dir)
	if got, err := Root(); err != nil || got != dir {
		t.Errorf("Root() = %q, %v; want %q", got, err, dir)
	}
	if got, err := KindDir(KindPinned); err != nil || got != filepath.Join(dir, KindPinned) {
		t.Errorf("KindDir(pinned) = %q, %v", got, err)
	}
}

func assertNoStaging(t *testing.T, kindDir string) {
	t.Helper()
	if left, _ := filepath.Glob(filepath.Join(kindDir, stagingPrefix+"*")); len(left) != 0 {
		t.Errorf("staging left behind: %v", left)
	}
}

// With no override the cache lives under the user cache directory, in forge's
// own subtree, so it never collides with another tool's cache.
func TestRoot_DefaultsToTheUserCacheDirectory(t *testing.T) {
	t.Setenv(EnvRoot, "")
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", base)
	t.Setenv("HOME", base)
	t.Setenv("LocalAppData", base)
	want, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(want, "forge", "tools")
	got, err := Root()
	if err != nil || got != want {
		t.Fatalf("Root() = %q, %v; want %q", got, err, want)
	}
	if dir, err := KindDir(KindGolangci); err != nil || dir != filepath.Join(want, KindGolangci) {
		t.Errorf("KindDir(golangci) = %q, %v; want it under %s", dir, err, want)
	}
}

// A machine with no user cache directory and no override gets an error that
// names the variable that fixes it, from Root and from everything built on it.
func TestRoot_NamesTheOverrideWhenThereIsNoCacheDirectory(t *testing.T) {
	t.Setenv(EnvRoot, "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("LocalAppData", "")
	if _, err := os.UserCacheDir(); err == nil {
		t.Skip("this OS finds a user cache directory without HOME")
	}
	if _, err := Root(); err == nil || !strings.Contains(err.Error(), EnvRoot) {
		t.Errorf("Root() error = %v; want it to name %s", err, EnvRoot)
	}
	if _, err := KindDir(KindGate); err == nil || !strings.Contains(err.Error(), EnvRoot) {
		t.Errorf("KindDir() error = %v; want Root's error passed through", err)
	}
}

// A published entry that is already ready is kept, and the finished stage is
// left for Ensure's cleanup: nothing is moved into or over it.
func TestPublish_KeepsAReadyEntry(t *testing.T) {
	t.Parallel()
	kindDir := t.TempDir()
	dir := filepath.Join(kindDir, "fake@v1")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tool), []byte("winner"), 0o755); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(kindDir, stagingPrefix+"fake@v1-x")
	if err := os.Mkdir(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, tool), []byte("loser"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := publish(stage, dir, readyWith("winner")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, tool)); string(b) != "winner" {
		t.Errorf("the published entry was overwritten: %q", b)
	}
	if _, err := os.Stat(filepath.Join(stage, tool)); err != nil {
		t.Errorf("publish consumed the stage of a lost race: %v", err)
	}
}

// If the rename fails because another install published the key in the
// meantime, the winner's entry is as good as ours and publish succeeds; if
// the rename fails with nothing usable in place, the error surfaces.
func TestPublish_RenameFailure(t *testing.T) {
	t.Parallel()
	kindDir := t.TempDir()
	missingStage := filepath.Join(kindDir, "no-such-stage")
	dir := filepath.Join(kindDir, "fake@v1")

	if err := publish(missingStage, dir, func(string) bool { return true }); err != nil {
		t.Errorf("rename lost to a ready entry: publish = %v, want nil", err)
	}
	err := publish(missingStage, dir, func(string) bool { return false })
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("rename failed with nothing ready: publish = %v, want the rename error", err)
	}
}

// An unusable entry that cannot be moved aside is an error naming it, rather
// than a rename that lands inside it.
func TestPublish_UnusableEntryThatCannotBeMovedAside(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory this user cannot rename within")
	}
	kindDir := t.TempDir()
	dir := filepath.Join(kindDir, "fake@v1")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(t.TempDir(), "stage")
	if err := os.Mkdir(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(kindDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(kindDir, 0o755) })

	err := publish(stage, dir, func(string) bool { return false })
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("publish = %v, want an error naming %s", err, dir)
	}
}

// A kind directory that cannot be created (its parent is a file) fails
// Ensure before it builds anything.
func TestEnsure_UnusableKindDirFailsBeforeBuilding(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	built := false
	_, err := Ensure(context.Background(), filepath.Join(parent, "gate"), "fake@v1", readyWith("v"), func(context.Context, string) error { built = true; return nil })
	if err == nil {
		t.Fatal("Ensure succeeded under a kind directory that is a file's child")
	}
	if built {
		t.Error("build ran although the staging directory could not be made")
	}
}

// The entry directory is world-readable even though MkdirTemp makes the stage
// 0700: the cache is shared by every user of the machine.
func TestEnsure_PublishedEntryIsTraversableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX permission bits")
	}
	t.Parallel()
	kindDir := t.TempDir()
	dir, err := Ensure(context.Background(), kindDir, "fake@v1", readyWith("v"), writeTool("v"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("entry mode = %o, want 755", got)
	}
}
