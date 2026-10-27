//go:build !windows

package toolcache

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes f's flock(2) without blocking, reporting false when another
// descriptor holds a conflicting one. flock is tied to the open file
// description, so the kernel releases it when f closes, including when the
// process dies: a killed install never wedges its key.
func tryLock(f *os.File, exclusive bool) (bool, error) {
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func unlock(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
