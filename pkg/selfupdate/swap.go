package selfupdate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// swapFS is the file operations that put a binary at an install path: the
// host's ([hostFS]), or a test's stand-in that models Windows' lock on a
// running image, so the Windows path is tested on every platform.
type swapFS struct {
	rename func(oldpath, newpath string) error
	remove func(path string) error
	// moveAside: the file at an install path may be a running image, which
	// Windows lets a process rename but neither replace nor remove. So it is
	// renamed to <stem>.old-<unixnano><ext> in the same directory first, and
	// removed as soon as nothing runs it ([sweepAside] retries at the next
	// start). Unix renames over a running binary, so it never moves one aside.
	moveAside bool
}

var hostFS = swapFS{rename: os.Rename, remove: os.Remove, moveAside: runningImageLocked}

// orHost is f, or the host's operations when f is the zero value.
func (f swapFS) orHost() swapFS {
	if f.rename == nil || f.remove == nil {
		return hostFS
	}
	return f
}

// replaceWithCopy puts a copy of src at dst by copy-to-sibling then rename:
// atomic, and a running dst keeps its old inode. Where a running image
// cannot be replaced (moveAside), dst is first renamed aside, named from now;
// if the copy then cannot take dst's name, dst is put back.
func (f swapFS) replaceWithCopy(src, dst string, now time.Time) error {
	dir := filepath.Dir(dst)
	tmp, err := copyToTemp(src, dir, "."+filepath.Base(dst)+".new-*")
	if err != nil {
		return fmt.Errorf("copy %s beside %s: %w", src, dst, err)
	}
	var aside string
	if f.moveAside {
		if aside, err = f.setAside(dst, now); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("move %s aside: %w", dst, err)
		}
	}
	if err := f.rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		err = fmt.Errorf("rename over %s: %w", dst, err)
		if aside != "" {
			// Never leave the install path empty.
			err = errors.Join(err, f.rename(aside, dst))
		}
		return err
	}
	if aside != "" {
		// Fails while aside is a running image; the next start sweeps it.
		_ = f.remove(aside)
	}
	return syncDir(dir)
}

// setAside renames path to a free <stem>.old-<unixnano><ext> beside it and
// returns that name, or "" when path does not exist. It never replaces a file:
// an earlier one may be another instance's running image.
func (f swapFS) setAside(path string, now time.Time) (string, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	for i := int64(0); ; i++ {
		aside := asideName(path, now.UnixNano()+i)
		if _, err := os.Lstat(aside); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err := f.rename(path, aside); err != nil {
			return "", err
		}
		return aside, nil
	}
}

// asideName is <dir>/<stem>.old-<ns><ext> for path <dir>/<stem><ext>:
// reliant.exe becomes reliant.old-<ns>.exe, still an .exe.
func asideName(path string, ns int64) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	return filepath.Join(filepath.Dir(path), fmt.Sprintf("%s.old-%d%s", strings.TrimSuffix(base, ext), ns, ext))
}

// isAsideOf reports whether name is an [asideName] of the binary named base.
func isAsideOf(base, name string) bool {
	ext := filepath.Ext(base)
	rest, ok := strings.CutPrefix(name, strings.TrimSuffix(base, ext)+".old-")
	if !ok || !strings.HasSuffix(rest, ext) {
		return false
	}
	digits := strings.TrimSuffix(rest, ext)
	if digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// sweepAside removes the images earlier swaps set aside beside exe. One that
// a process still runs cannot be removed and stays for a later start.
func sweepAside(f swapFS, exe string) {
	if !f.moveAside || exe == "" {
		return
	}
	dir, base := filepath.Dir(exe), filepath.Base(exe)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Type().IsRegular() && isAsideOf(base, e.Name()) {
			_ = f.remove(filepath.Join(dir, e.Name()))
		}
	}
}
