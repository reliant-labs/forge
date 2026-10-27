package toolcache

import (
	"os"

	"golang.org/x/sys/unix"
)

func openLockNoFollow(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat2(int(parent.Fd()), name, &unix.OpenHow{Flags: unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// openat2 NO_XDEV rejects bind mounts as well as different-device mounts.
// On a kernel/filesystem without these guarantees, callers retain the tree.
func openDirectoryNoFollow(parent *os.File, name string, confined bool) (*os.File, error) {
	resolve := uint64(unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS)
	if confined {
		resolve |= unix.RESOLVE_NO_XDEV
	}
	fd, err := unix.Openat2(int(parent.Fd()), name, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: resolve,
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
