package instance

import (
	"fmt"
	"os"
	"path/filepath"
)

// lockRel is the global instance/port lock. A single lock guards BOTH the
// index registry and the per-instance port-store allocation, because the
// two are one atomic operation from the user's view ("claim my slot"): the
// concurrent first-`up` of two worktrees must serialize so they get
// distinct indices AND distinct port blocks. One lock, not two, so the
// ordering can never deadlock.
const lockRel = ".forge/instances.lock"

// withLock runs fn while holding the exclusive, cross-process instance
// lock. The lock is advisory (flock(2)) and auto-releases when the holding
// process exits or the fd closes — so a crashed `forge up` never leaves a
// stale lock the next run has to time out on, unlike an O_EXCL marker file.
// Blocking (LOCK_EX without LOCK_NB): a second worktree's `up` waits for
// the first to finish its claim, then proceeds — exactly the serialization
// the index/port assignment needs.
func withLock(projectDir string, fn func() error) error {
	p := filepath.Join(projectDir, lockRel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("create .forge dir: %w", err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open instance lock %s: %w", p, err)
	}
	defer f.Close()

	if err := lockFD(f); err != nil {
		return fmt.Errorf("acquire instance lock %s: %w", p, err)
	}
	defer func() { _ = unlockFD(f) }()

	return fn()
}
