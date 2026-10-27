package toolcache

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCleanupRefusesBindMount(t *testing.T) {
	// The namespace and both directories are disposable; never mount or remove
	// anything in the machine cache. Unprivileged runners explicitly skip.
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	tree := filepath.Join(base, "tree")
	mount := filepath.Join(tree, "mounted")
	for _, p := range []string{outside, mount} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("outside"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount(outside, mount, "", unix.MS_BIND, ""); err != nil {
		t.Skipf("bind mounts unavailable: %v", err)
	}
	defer func() {
		if err := unix.Unmount(mount, 0); err != nil {
			t.Errorf("unmount test fixture: %v", err)
		}
	}()
	if err := removeAllWritable(tree); err == nil {
		t.Fatal("bind mount traversal accepted")
	}
	info, err := os.Stat(sentinel)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("outside mode changed: %v %v", info, err)
	}
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "outside" {
		t.Fatalf("outside data changed: %q %v", b, err)
	}
}
