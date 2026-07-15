package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestAcquireGenerateLock(t *testing.T) {
	dir := t.TempDir()

	release, err := acquireGenerateLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Lock file should exist
	lockPath := filepath.Join(dir, ".forge", "forge.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}

	// Second acquire should fail
	_, err = acquireGenerateLock(dir)
	if err == nil {
		t.Fatal("expected error on second acquire, got nil")
	}

	// Release and re-acquire should succeed
	release()
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file should be removed after release, got err: %v", err)
	}

	release2, err := acquireGenerateLock(dir)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	release2()
}

func TestAcquireGenerateLock_StaleLock(t *testing.T) {
	dir := t.TempDir()

	// Create a stale lock file manually
	lockDir := filepath.Join(dir, ".forge")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, "forge.lock")
	if err := os.WriteFile(lockPath, []byte("pid=99999\ntime=old\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Backdate the file to make it stale
	staleTime := time.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	// Should succeed because stale lock is removed
	release, err := acquireGenerateLock(dir)
	if err != nil {
		t.Fatalf("acquire with stale lock: %v", err)
	}
	release()
}

// TestAcquireGenerateLock_ReclaimsDeadOwnerPID pins the F9 fix: a lock left
// by a process that has since died (the daemon disconnecting mid-generate) is
// reclaimed IMMEDIATELY — no 10-minute staleness wait, no manual `rm`. The
// lock's mtime is FRESH so a reclaim can only be the PID-liveness path.
func TestAcquireGenerateLock_ReclaimsDeadOwnerPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("processAlive is conservative on Windows; reclaim falls back to the age gate")
	}
	dir := t.TempDir()
	lockDir := filepath.Join(dir, ".forge")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, "forge.lock")

	// Run+reap a subprocess so its PID is guaranteed dead.
	sub := exec.Command("/bin/sh", "-c", "exit 0")
	if err := sub.Run(); err != nil {
		t.Fatalf("spawn helper process: %v", err)
	}
	deadPID := sub.Process.Pid

	// A FRESH lock (not backdated) owned by the dead PID.
	body := fmt.Sprintf("pid=%d\ntime=%s\n", deadPID, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(lockPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	release, err := acquireGenerateLock(dir)
	if err != nil {
		t.Fatalf("acquire should reclaim a fresh lock owned by a dead PID, got: %v", err)
	}
	release()
}

// TestAcquireGenerateLock_KeepsLiveOwnerLock is the guard rail: a fresh lock
// owned by a LIVE process must NOT be reclaimed (a concurrent generate holds
// it legitimately).
func TestAcquireGenerateLock_KeepsLiveOwnerLock(t *testing.T) {
	dir := t.TempDir()
	lockDir := filepath.Join(dir, ".forge")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, "forge.lock")

	// Our own PID is, by definition, alive; a fresh lock owned by it stands.
	body := fmt.Sprintf("pid=%d\ntime=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	if err := os.WriteFile(lockPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := acquireGenerateLock(dir); err == nil {
		t.Fatal("expected acquire to fail on a fresh lock owned by a live process, got nil")
	}
}
