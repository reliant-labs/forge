//go:build !windows

package forgedelegate

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// entryLocks says lockEntry really excludes other processes, so the holder
// may treat what it finds in the entry as abandoned.
const entryLocks = true

// lockEntryPoll is how often a waiter retries the entry's lock.
var lockEntryPoll = 250 * time.Millisecond

// lockEntry takes an exclusive flock(2) on path, waiting until it is free or
// ctx ends. The kernel drops the lock when the holder exits, however it
// exits, so a killed builder never wedges the entry.
func lockEntry(ctx context.Context, path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(lockEntryPoll):
		}
	}
}
