//go:build linux

package storage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcAgeFromStat(t *testing.T) {
	// comm contains spaces and a parenthesis; start time (field 22) is 12345 ticks.
	stat := "4242 (my (odd) cmd) S 1 4242 4242 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 1 0 12345 1000000 100 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"
	age, ok := procAgeFromStat(stat, time.Hour)
	if want := time.Hour - 123*time.Second - 450*time.Millisecond; !ok || age != want {
		t.Fatalf("age = %v, %v; want %v", age, ok, want)
	}
	if _, ok := procAgeFromStat("garbage", time.Hour); ok {
		t.Error("parsed garbage")
	}
}

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
	cmd, stop := startProcExecutableChild(t, bin, false)
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

	if err := stop(5 * time.Second); err != nil {
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

// startProcExecutableChild gives the child exactly one waiter. stop and cleanup
// share its result; neither can double-Wait or abandon the child on an EOF hang.
func startProcExecutableChild(t *testing.T, bin string, ignoreEOF bool) (*exec.Cmd, func(time.Duration) error) {
	t.Helper()
	mode := "release-on-eof"
	if ignoreEOF {
		mode = "ignore-eof"
	}
	cmd := exec.Command(bin, "-test.run=^TestProcExecutableOwnedChild$", "--", mode, "forge-proc-executable-child")
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
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	stopped := false // stop and cleanup run serially in the parent test.
	stop := func(grace time.Duration) error {
		if stopped {
			return errors.New("owned child already stopped")
		}
		stopped = true
		_ = release.Close()
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-done:
			return waitErr
		case <-timer.C:
		}
		// A timeout is always an error, even when killing and reaping succeeds.
		// Kill the exact child handle, never a process name or process group.
		killErr := cmd.Process.Kill()
		reap := time.NewTimer(5 * time.Second)
		defer reap.Stop()
		select {
		case <-done:
			return fmt.Errorf("owned child ignored EOF: %w (kill: %v; wait: %v)", context.DeadlineExceeded, killErr, waitErr)
		case <-reap.C:
			return fmt.Errorf("owned child was not reaped after kill: %w (kill: %v)", context.DeadlineExceeded, killErr)
		}
	}
	t.Cleanup(func() {
		if !stopped {
			if err := stop(5 * time.Second); err != nil {
				t.Errorf("owned child cleanup: %v", err)
			}
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
	return cmd, stop
}

func TestProcExecutableChildShutdownIsBoundedAndIsolated(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sibling, stopSibling := startProcExecutableChild(t, self, false)
	stuck, stopStuck := startProcExecutableChild(t, self, true)
	started := time.Now()
	if err := stopStuck(20 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an EOF-ignoring child must report a shutdown timeout, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("killing and reaping the stuck child took %v", elapsed)
	}
	if stuck.ProcessState == nil {
		t.Fatal("shutdown did not reap the stuck child")
	}
	status, ok := stuck.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("stuck child was not killed: %v", stuck.ProcessState)
	}
	if _, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", stuck.Process.Pid)); !os.IsNotExist(err) {
		t.Fatalf("stuck child's exe still exists after shutdown: %v", err)
	}
	if _, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", sibling.Process.Pid)); err != nil {
		t.Fatalf("stopping the stuck child affected its sibling: %v", err)
	}
	if err := stopSibling(5 * time.Second); err != nil {
		t.Fatalf("sibling did not exit cleanly on its own EOF: %v", err)
	}
}

// TestProcExecutableOwnedChild runs only when explicitly invoked by a parent.
// EOF releases the normal helper; ignore-eof exercises bounded shutdown.
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
	if os.Args[len(os.Args)-2] == "ignore-eof" {
		for {
			time.Sleep(time.Hour)
		}
	}
}

// The process can start after a scan's initial uptime snapshot. A FIFO lets
// the fake process publish stat only after advancing uptime, without sleeps.
func TestReadProcExecutablesSamplesUptimeAfterProcessStat(t *testing.T) {
	proc, root := t.TempDir(), t.TempDir()
	pid := filepath.Join(proc, "100")
	if err := os.Mkdir(pid, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "ab", hexName(4, "-d"), "forge"), filepath.Join(pid, "exe")); err != nil {
		t.Fatal(err)
	}
	uptime := filepath.Join(proc, "uptime")
	if err := os.WriteFile(uptime, []byte("100.00 0.00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stat := filepath.Join(pid, "stat")
	if err := syscall.Mkfifo(stat, 0o600); err != nil {
		t.Fatal(err)
	}
	published := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(stat, os.O_WRONLY, 0)
		if err != nil {
			published <- err
			return
		}
		defer f.Close()
		if err := os.WriteFile(uptime, []byte("100.01 0.00\n"), 0o644); err != nil {
			published <- err
			return
		}
		_, err = fmt.Fprint(f, procAgeTestStat("10001"))
		published <- err
	}()
	got, err := readProcExecutables(context.Background(), proc, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Age != 0 {
		t.Fatalf("fresh process after stale snapshot: %v; want one process aged zero, not the unknown-age lease", got)
	}
}

func procAgeTestStat(ticks string) string {
	// State is field 3, followed by 18 unused fields, then starttime (22).
	return "100 (fresh child) S " + strings.Repeat("0 ", 18) + ticks + "\n"
}

func TestProcExecutableAgeInvalidMetadataFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, uptime, stat string
	}{
		{"missing stat", "100.00 0.00\n", ""},
		{"malformed stat", "100.00 0.00\n", "garbage"},
		{"malformed ticks", "100.00 0.00\n", procAgeTestStat("bad")},
		{"future start", "100.00 0.00\n", procAgeTestStat("10001")},
		{"malformed uptime", "garbage", procAgeTestStat("9000")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proc, root := t.TempDir(), t.TempDir()
			pid := filepath.Join(proc, "100")
			if err := os.Mkdir(pid, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, "ab", hexName(4, "-d"), "forge"), filepath.Join(pid, "exe")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(proc, "uptime"), []byte(tc.uptime), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.stat != "" {
				if err := os.WriteFile(filepath.Join(pid, "stat"), []byte(tc.stat), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readProcExecutables(context.Background(), proc, root)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Age != goCacheLongLived {
				t.Fatalf("unknown process age must keep the fail-closed lease: %v", got)
			}
		})
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
