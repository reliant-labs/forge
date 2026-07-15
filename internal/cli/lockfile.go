package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const lockStaleDuration = 10 * time.Minute

// acquireGenerateLock creates .forge/forge.lock in the project directory.
// Returns a cleanup function to release the lock.
//
// A held lock is reclaimed when its owner is gone — the PID recorded in the
// file is no longer a live process (the classic case: the daemon disconnected
// or was killed mid-`forge generate`, so its defer never removed the lock) —
// or, as a fallback for PIDs we can't probe (Windows, a recycled PID), when
// the file is older than lockStaleDuration. Reclaiming on a dead PID is
// immediate: the next generate no longer has to wait out the 10-minute
// staleness window or be told to `rm` the lock by hand (fr F9).
func acquireGenerateLock(projectDir string) (release func(), err error) {
	lockDir := filepath.Join(projectDir, ".forge")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, fmt.Errorf("create .forge directory: %w", err)
	}

	lockPath := filepath.Join(lockDir, "forge.lock")

	// One reclaim attempt: create → on collision, decide whether to reclaim,
	// then retry the create exactly once. A second collision means a live
	// forge genuinely holds the lock.
	for attempt := 0; attempt < 2; attempt++ {
		f, createErr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if createErr == nil {
			// Write PID and timestamp so a later run can probe liveness.
			_, _ = fmt.Fprintf(f, "pid=%d\ntime=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !os.IsExist(createErr) {
			return nil, fmt.Errorf("create lock file: %w", createErr)
		}
		if attempt == 0 && reclaimAbandonedLock(lockPath) {
			continue // reclaimed — retry the exclusive create
		}
		return nil, fmt.Errorf(
			"another forge process is running in this project (lock: %s). "+
				"If it is stale, remove it with: rm %s", lockPath, lockPath)
	}
	// Unreachable: the loop returns on every path.
	return nil, fmt.Errorf("failed to acquire generate lock: %s", lockPath)
}

// reclaimAbandonedLock removes the lock file when it is safe to take over.
// Two independent conditions, either of which reclaims:
//
//   - the recorded owner PID is a dead process (the F9 fix: a disconnected
//     daemon's lock is reclaimed immediately, no waiting), OR
//   - the file is older than lockStaleDuration (the pre-existing age gate,
//     kept as the safety net for legacy locks with no PID and for a PID that
//     has been recycled to an unrelated live process).
//
// An owner PID that is still alive AND a lock younger than the staleness
// window is left untouched, so a legitimately long-running generate is never
// yanked. Note the age gate matching the historical behavior means a >10min
// live generate can still be reclaimed exactly as before — that trade-off is
// unchanged by this function. Returns true when the lock was removed.
func reclaimAbandonedLock(lockPath string) bool {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		// Vanished between the failed create and this read — another process
		// may have just released it. Treat as reclaimable so we retry the
		// create; a genuine race just re-collides on the next attempt.
		return os.IsNotExist(err)
	}

	// Dead-owner reclaim: only for a PID that is BOTH parseable and not our
	// own (a live self-PID means we already hold it — never reclaim that).
	if pid, ok := lockOwnerPID(data); ok && pid != os.Getpid() && !processAlive(pid) {
		fmt.Fprintf(os.Stderr, "warning: reclaiming abandoned lock %s (owner pid %d is gone)\n", lockPath, pid)
		return os.Remove(lockPath) == nil
	}

	// Age reclaim (owner alive/unknown): the historical staleness window.
	if info, statErr := os.Stat(lockPath); statErr == nil {
		if age := time.Since(info.ModTime()); age > lockStaleDuration {
			fmt.Fprintf(os.Stderr, "warning: removing stale lock file %s (age: %s)\n", lockPath, age.Round(time.Second))
			return os.Remove(lockPath) == nil
		}
	}
	return false
}

// lockOwnerPID extracts the pid recorded in a lock file body of the form
// "pid=<n>\ntime=<rfc3339>\n". Returns (0, false) when no pid line parses.
func lockOwnerPID(body []byte) (int, bool) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "pid=")
		if !ok {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil && pid > 0 {
			return pid, true
		}
	}
	return 0, false
}
