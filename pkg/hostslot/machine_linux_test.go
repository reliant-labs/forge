//go:build linux

package hostslot

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// delegationBoundary reads systemd's user.delegate marker. cgroupfs is not
// writable here, so the marker goes on a temp directory; a filesystem without
// user xattrs skips.
func TestDelegationBoundary(t *testing.T) {
	plain, marked, zero := t.TempDir(), t.TempDir(), t.TempDir()
	if err := unix.Setxattr(marked, "user.delegate", []byte("1"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Skipf("the temp dir's filesystem has no user xattrs: %v", err)
		}
		t.Fatal(err)
	}
	if err := unix.Setxattr(zero, "user.delegate", []byte("0"), 0); err != nil {
		t.Fatal(err)
	}
	if !delegationBoundary(marked) {
		t.Error("user.delegate=1 not read as a delegation boundary")
	}
	if delegationBoundary(plain) || delegationBoundary(zero) {
		t.Error("an unmarked directory, or user.delegate=0, read as a delegation boundary")
	}
	if delegationBoundary(marked + "/missing") {
		t.Error("a missing directory read as a delegation boundary")
	}
}
