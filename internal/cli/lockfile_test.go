package cli

import (
	"os"
	"path/filepath"
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
