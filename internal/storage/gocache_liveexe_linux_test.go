//go:build linux

package storage

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The real /proc: a child process running a binary copied into an executable
// cache entry is found, and is gone from the list once it exits.
func TestProcExecutablesFindsAProcessRunningFromTheCache(t *testing.T) {
	root := t.TempDir()
	makeBuildCache(t, root)
	dir := filepath.Join(root, "9f", hexName(7, "-d"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "sleeper")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, src, 0o755); err != nil {
		t.Fatal(err)
	}
	// The copied helper announces readiness, then waits for EOF from this
	// parent, not a wall-clock deadline. A slow /proc scan must not outlive it.
	cmd := exec.Command(bin, "-test.run=^TestProcExecutableOwnedChild$", "--", "forge-proc-executable-child")
	cmd.Stderr = os.Stderr
	release, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = release.Close() })
	ready, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ready.Close() })
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill() // Only the child this test started.
			_ = cmd.Wait()
		}
	})
	readiness := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(ready).ReadString('\n')
		if err == nil && line != "ready\n" {
			err = fmt.Errorf("unexpected child readiness: %q", line)
		}
		readiness <- err
	}()
	select {
	case err := <-readiness:
		if err != nil {
			t.Fatalf("child did not become ready: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not announce readiness")
	}
	waitForExe(t, cmd.Process.Pid, func(exe string) bool { return exe == bin })

	exes, err := procExecutables(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	waitForExe(t, cmd.Process.Pid, func(exe string) bool { return exe == bin })
	var found *LiveExecutable
	for i := range exes {
		if _, ok := cacheEntryOfExecutable(root, exes[i].Path); ok {
			found = &exes[i]
		}
	}
	if found == nil {
		link, lerr := os.Readlink(fmt.Sprintf("/proc/%d/exe", cmd.Process.Pid))
		t.Fatalf("the running process was not found in %v; /proc/%d/exe -> %q (%v); root %s; uid %d", exes, cmd.Process.Pid, link, lerr, root, os.Getuid())
	}
	if found.Age < 0 || found.Age > time.Minute {
		t.Errorf("age = %v for a process started a moment ago", found.Age)
	}

	// Deleted from under the process: still reported, still protecting its entry.
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	waitForExe(t, cmd.Process.Pid, func(exe string) bool { return exe == bin+" (deleted)" })
	exes, err = procExecutables(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	waitForExe(t, cmd.Process.Pid, func(exe string) bool { return exe == bin+" (deleted)" })
	if len(exes) != 1 || exes[0].Path != bin+" (deleted)" {
		link, lerr := os.Readlink(fmt.Sprintf("/proc/%d/exe", cmd.Process.Pid))
		t.Fatalf("the owned process whose binary was unlinked is not reported exactly once: %v; /proc/%d/exe -> %q (%v)", exes, cmd.Process.Pid, link, lerr)
	}
	if !strings.HasSuffix(exes[0].Path, " (deleted)") {
		t.Errorf("a process whose binary was unlinked is not reported as such: %v", exes)
	}
	if key, ok := cacheEntryOfExecutable(root, exes[0].Path); !ok || key != "9f/"+hexName(7, "-d") {
		t.Errorf("the deleted binary does not map to its entry: %q, %v", key, ok)
	}

	if err := release.Close(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatalf("owned child did not exit cleanly: %v", err)
	}
	if _, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", cmd.Process.Pid)); !os.IsNotExist(err) {
		t.Fatalf("reaped child's exe still exists, or could not be inspected: %v", err)
	}
	exes, err = procExecutables(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(exes) != 0 {
		t.Fatalf("exited child still protects its cache entry: %v", exes)
	}
}

// TestProcExecutableOwnedChild runs only when explicitly invoked by the parent
// above. The parent owns its lifetime through stdin; no external binary or timer
// can cause it to disappear while the cache inspector is scanning /proc.
func TestProcExecutableOwnedChild(t *testing.T) {
	if os.Args[len(os.Args)-1] != "forge-proc-executable-child" {
		return
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

// An unreadable /proc is an error, never an empty list.
func TestReadProcExecutablesFailsClosedWhenProcIsMissing(t *testing.T) {
	if _, err := readProcExecutables(context.Background(), filepath.Join(t.TempDir(), "no-proc"), t.TempDir()); err == nil {
		t.Fatal("a missing /proc must be an error so the trim keeps every executable entry")
	}
}

// A fake /proc: only processes under the cache are returned, whatever else runs.
func TestReadProcExecutablesKeepsOnlyCacheExecutables(t *testing.T) {
	proc, root := t.TempDir(), t.TempDir()
	hash := hexName(4, "-d")
	inside := filepath.Join(root, "ab", hash, "forge")
	for pid, exe := range map[string]string{"100": inside, "101": "/usr/bin/bash", "102": inside + " (deleted)"} {
		if err := os.MkdirAll(filepath.Join(proc, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, filepath.Join(proc, pid, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(proc, "uptime"), []byte("1000.00 900.00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(proc, "self-not-a-pid"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := readProcExecutables(context.Background(), proc, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want the two processes running from the cache", got)
	}
	for _, e := range got {
		if e.Age != goCacheLongLived {
			t.Errorf("a process with no readable start time must count as old as any build that counts, got %v", e.Age)
		}
	}
}

// A process whose exe the kernel will not show (another user's, or any
// non-dumpable one) must not fail the listing, or one such process on the
// machine would disable executable trimming for good. Skipped when running as
// root, which can read everything.
func TestReadProcExecutablesSkipsProcessesItCannotInspect(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read every exe")
	}
	// /proc/<pid>/exe is a symlink; an unreadable one is simulated by a
	// directory in its place, which readlink answers with EINVAL (not a
	// permission error), so use a real kernel-refused read instead: PID 1.
	if _, err := os.Readlink("/proc/1/exe"); err == nil {
		t.Skip("this machine lets us read PID 1's exe")
	}
	if _, err := readProcExecutables(context.Background(), "/proc", t.TempDir()); err != nil {
		t.Fatalf("a process we cannot inspect failed the whole listing: %v", err)
	}
}

// waitForExe polls /proc/<pid>/exe until ok accepts it, failing with the last
// value seen.
func waitForExe(t *testing.T, pid int, ok func(string) bool) {
	t.Helper()
	var last string
	var lerr error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		last, lerr = os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if lerr == nil && ok(last) {
			return
		}
	}
	t.Fatalf("/proc/%d/exe never became the expected path; last %q (%v)", pid, last, lerr)
}
