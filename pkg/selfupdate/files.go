package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path through a sibling temp file, so a
// crash leaves the old content or the new, never a torn file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := finishFile(f, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// finishFile sets perm, fsyncs and closes f.
func finishFile(f *os.File, perm os.FileMode) error {
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// copyToTemp copies src into a new temp file in dir named after pattern,
// fsynced and executable, and returns its path. The caller renames or
// removes it.
func copyToTemp(src, dir, pattern string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(out.Name())
		return "", err
	}
	if err := finishFile(out, 0o755); err != nil {
		_ = os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// replaceWithCopy puts a copy of src at dst by copy-to-sibling then rename:
// atomic, and a running dst keeps its old inode.
func replaceWithCopy(src, dst string) error {
	dir := filepath.Dir(dst)
	tmp, err := copyToTemp(src, dir, "."+filepath.Base(dst)+".new-*")
	if err != nil {
		return fmt.Errorf("copy %s beside %s: %w", src, dst, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename over %s: %w", dst, err)
	}
	return syncDir(dir)
}

// preserve saves src at dst: a hard link when both share a filesystem (the
// old inode then survives the swap for free), else a copy.
func preserve(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return syncDir(filepath.Dir(dst))
	}
	return replaceWithCopy(src, dst)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
