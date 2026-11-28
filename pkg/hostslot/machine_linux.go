//go:build linux

package hostslot

import (
	"bytes"
	"os"

	"golang.org/x/sys/unix"
)

func totalMemory() (uint64, string) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, ""
	}
	return meminfoTotal(data), "/proc/meminfo"
}

func platformCgroupLimits() (cgroupLimit, bool) {
	return readCgroupV2("/sys/fs/cgroup", "/proc/self/cgroup")
}

func platformPoolCgroupLimits() (cgroupLimit, bool) {
	return readCgroupV2Pool("/sys/fs/cgroup", "/proc/self/cgroup", delegationBoundary)
}

// delegateXattrs are the extended attributes that mark a cgroup v2
// delegation boundary: systemd sets them to "1" on a unit's cgroup when the
// unit has Delegate=yes (systemd's CGROUP_DELEGATION.md). user.delegate is
// readable without privilege; trusted.delegate only as root, and older
// systemd set only that one.
var delegateXattrs = []string{"user.delegate", "trusted.delegate"}

// delegationBoundary reports whether the cgroup directory dir is marked as
// delegated. Any error (no such attribute, a filesystem without xattrs, no
// permission) is "not marked".
func delegationBoundary(dir string) bool {
	buf := make([]byte, 16)
	for _, name := range delegateXattrs {
		n, err := unix.Getxattr(dir, name, buf)
		if err == nil && string(bytes.TrimSpace(buf[:n])) == "1" {
			return true
		}
	}
	return false
}
