package toolcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
// generate pins protoc-gen-go). One builds; the rest wait and use its result.
func TestEnsure_ConcurrentMissesBuildOnce(t *testing.T) {
	t.Parallel()
	kindDir := t.TempDir()
	want := strings.Repeat("x", 32)
	var builds atomic.Int32
	build := func(ctx context.Context, stage string) error {
		builds.Add(1)
		return writeTool(want)(ctx, stage)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir, err := Ensure(context.Background(), kindDir, "fake@v1", readyWith(want), build)
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
	if n := builds.Load(); n != 1 {
		t.Errorf("%d builds ran for one key; concurrent misses must share one", n)
	}
	assertNoStaging(t, kindDir)
}

// The lock spans processes, which is the case that matters: separate forge
// commands in separate worktrees. Each helper process is this test binary.
func TestEnsure_ConcurrentProcessesBuildOnce(t *testing.T) {
	t.Parallel()
	kindDir := t.TempDir()
	count := filepath.Join(t.TempDir(), "builds")
	var cmds []*exec.Cmd
	for range 4 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperEnsureProcess$")
		cmd.Env = append(os.Environ(), "TOOLCACHE_HELPER_KIND="+kindDir, "TOOLCACHE_HELPER_COUNT="+count)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper process: %v", err)
		}
	}
	b, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "build\n"); n != 1 {
		t.Errorf("%d builds across 4 processes; want 1", n)
	}
}

// TestHelperEnsureProcess is one forge process in the cross-process test.
func TestHelperEnsureProcess(t *testing.T) {
	kindDir := os.Getenv("TOOLCACHE_HELPER_KIND")
	if kindDir == "" {
		t.Skip("helper process for TestEnsure_ConcurrentProcessesBuildOnce")
	}
	want := strings.Repeat("p", 32)
	build := func(ctx context.Context, stage string) error {
		f, err := os.OpenFile(os.Getenv("TOOLCACHE_HELPER_COUNT"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, err = f.WriteString("build\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		return writeTool(want)(ctx, stage)
	}
	dir, err := Ensure(context.Background(), kindDir, "fake@v1", readyWith(want), build)
	if err != nil {
		t.Fatal(err)
	}
	if !readyWith(want)(dir) {
		t.Fatalf("%s is not a complete entry", dir)
	}
}

// A reader that does not take the lock (an older forge, a shell) still never
// sees a half-written binary: the entry appears by one rename.
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
	if _, err := os.Stat(filepath.Join(kindDir, "fake@v1", UsedMarker)); err != nil {
		t.Errorf("a published entry carries its last-use marker: %v", err)
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
	if left, _ := filepath.Glob(filepath.Join(kindDir, evictingPrefix+"*")); len(left) != 0 {
		t.Errorf("the replaced entry was left behind: %v", left)
	}
}

// Every resolution records the use, which is what keeps Evict off an entry
// in use.
func TestEnsure_HitRecordsTheUse(t *testing.T) {
	t.Parallel()
	kindDir := t.TempDir()
	if _, err := Ensure(context.Background(), kindDir, "fake@v1", readyWith("v"), writeTool("v")); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(kindDir, "fake@v1", UsedMarker)
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	built := false
	if _, err := Ensure(context.Background(), kindDir, "fake@v1", readyWith("v"), func(context.Context, string) error { built = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if built {
		t.Error("a ready entry was rebuilt")
	}
	if info, err := os.Stat(marker); err != nil || time.Since(info.ModTime()) > time.Hour {
		t.Errorf("a hit did not touch the last-use marker (%v)", err)
	}
}

// A caller waiting on another install gives up when its context does.
func TestEnsure_WaitingHonoursTheContext(t *testing.T) {
	t.Parallel()
	kindDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(kindDir, lockDir), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPath(kindDir, "fake@v1"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if ok, err := tryLock(f, true); !ok || err != nil {
		t.Fatalf("take the lock: %v %v", ok, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = Ensure(ctx, kindDir, "fake@v1", readyWith("v"), writeTool("v"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; want the context's deadline", err)
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
