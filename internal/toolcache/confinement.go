package toolcache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/openfiles"
)

// cacheDir pins a directory inode. Children are opened without following
// links, and (below the cache root) without crossing mount boundaries.
// Path-based chmod is deliberately never used: even Root.Chmod can race a
// symlink replacement on Unix. Permissions are changed through an open fd.
type cacheDir struct {
	root *os.Root
	file *os.File
}

func (d *cacheDir) close() { _ = d.root.Close(); _ = d.file.Close() }

func openCacheDir(path string) (*cacheDir, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(abs)
	base := volume + string(filepath.Separator)
	r, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	f, err := r.Open(".")
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	d := &cacheDir{r, f}
	for _, part := range strings.Split(strings.TrimPrefix(abs, base), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		next, err := d.child(part, false)
		d.close()
		if err != nil {
			return nil, err
		}
		d = next
	}
	return d, nil
}

// openRegular opens an existing lock file without following a replaced link.
func (d *cacheDir) openRegular(name string) (*os.File, error) {
	before, err := d.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("retain %s: not a regular lock", name)
	}
	f, err := openLockNoFollow(d.file, name)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(before, actual) {
		_ = f.Close()
		return nil, fmt.Errorf("retain %s: lock inode changed", name)
	}
	return f, nil
}

func (d *cacheDir) child(name string, confined bool) (*cacheDir, error) {
	before, err := d.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("retain %s: not a plain directory", name)
	}
	f, err := openDirectoryNoFollow(d.file, name, confined)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(before, actual) {
		_ = f.Close()
		return nil, fmt.Errorf("retain %s: directory identity changed", name)
	}
	r, err := d.root.OpenRoot(name)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	check, err := r.Stat(".")
	if err != nil || !os.SameFile(actual, check) {
		_ = r.Close()
		_ = f.Close()
		return nil, fmt.Errorf("retain %s: root identity changed", name)
	}
	return &cacheDir{r, f}, nil
}

// parent returns a pinned parent and a single leaf name under d. Every
// intermediate component must remain an ordinary directory on this mount.
func (d *cacheDir) parent(relative string) (*cacheDir, string, error) {
	if !filepath.IsLocal(relative) || relative == "." {
		return nil, "", fmt.Errorf("retain unsafe cache path %q", relative)
	}
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	current, err := d.child(".", true)
	if err != nil {
		return nil, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		next, err := current.child(part, true)
		current.close()
		if err != nil {
			return nil, "", err
		}
		current = next
	}
	return current, parts[len(parts)-1], nil
}

// validateTree refuses a mount, unreadable directory or changed inode before
// any chmod/unlink. Symlinks are leaves, never paths to traverse.
func (d *cacheDir) validateTree(name string) error {
	info, err := d.root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	sub, err := d.child(name, true)
	if err != nil {
		return err
	}
	defer sub.close()
	children, err := sub.file.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, c := range children {
		if err := sub.validateTree(c.Name()); err != nil {
			return err
		}
	}
	return nil
}

// removeTree unlinks through pinned parents only. authorize can pace/charge
// each individual unlink without gaining authority to traverse paths itself.
func (d *cacheDir) removeTree(name, display string, authorize func(string, func() error) error) error {
	info, err := d.root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		sub, err := d.child(name, true)
		if err != nil {
			return err
		}
		defer sub.close()
		if err := sub.file.Chmod(info.Mode().Perm() | 0700); err != nil {
			return err
		}
		children, err := sub.file.ReadDir(-1)
		if err != nil {
			return err
		}
		for _, c := range children {
			if err := sub.removeTree(c.Name(), filepath.Join(display, c.Name()), authorize); err != nil {
				return err
			}
		}
	}
	unlink := func() error {
		current, err := d.root.Lstat(name)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !os.SameFile(info, current) {
			return fmt.Errorf("retain %s: inode changed before unlink", display)
		}
		return d.root.Remove(name) // removes symlinks themselves, never their targets
	}
	if authorize != nil {
		return authorize(display, unlink)
	}
	return unlink()
}

func removeAllWritable(path string) error {
	// Rejected-entry cleanup can be reached from publish, outside an eviction
	// pass. It still needs live-executable/open-file evidence; errors retain.
	snap, err := openfiles.Take(context.Background())
	if err != nil || snap.Holds(path) {
		return fmt.Errorf("retain %s: in use or process inspection unavailable", path)
	}
	return removeWritableConfined(path)
}

func removeWritableConfined(path string) error {
	parent, err := openCacheDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.close()
	name := filepath.Base(path)
	if err := parent.validateTree(name); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return parent.removeTree(name, path, nil)
}
