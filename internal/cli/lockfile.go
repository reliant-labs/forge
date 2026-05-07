package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const lockStaleDuration = 10 * time.Minute

// acquireGenerateLock creates .forge/forge.lock in the project directory.
// Returns a cleanup function to release the lock. If the lock already exists
// and is stale (older than 10 minutes), it is forcefully removed.
func acquireGenerateLock(projectDir string) (release func(), err error) {
	lockDir := filepath.Join(projectDir, ".forge")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return nil, fmt.Errorf("create .forge directory: %w", err)
	}

	lockPath := filepath.Join(lockDir, "forge.lock")

	// Check for stale lock (> 10 minutes old)
	if info, err := os.Stat(lockPath); err == nil {
		age := time.Since(info.ModTime())
		if age > lockStaleDuration {
			fmt.Fprintf(os.Stderr, "warning: removing stale lock file %s (age: %s)\n", lockPath, age.Round(time.Second))
			os.Remove(lockPath)
		}
	}

	// Try to create exclusively
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("another forge process is running in this project (lock: %s). If this is stale, remove it manually", lockPath)
		}
		return nil, fmt.Errorf("create lock file: %w", err)
	}

	// Write PID and timestamp for debugging
	fmt.Fprintf(f, "pid=%d\ntime=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	f.Close()

	release = func() {
		os.Remove(lockPath)
	}
	return release, nil
}
