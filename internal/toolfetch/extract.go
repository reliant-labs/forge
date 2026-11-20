package toolfetch

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// extractMember writes the regular file whose base name is member, out of
// the archive at archivePath (a .zip or a .tar.gz, decided by assetName), to
// dest with mode 0755. The archive's directory layout is flattened: a layout
// change fails here, once, by name. The archive has already been verified
// against its pinned sha256, so this guards against a malformed entry, not a
// hostile one; the copy is still bounded by the entry's declared size and by
// maxArchiveBytes.
func extractMember(archivePath, assetName, member, dest string) error {
	return readMember(archivePath, assetName, member, func(src io.Reader, size int64) error {
		return writeExecutable(src, size, member, dest)
	})
}

// readMember also lets cache validation hash the pinned archive's member
// without extracting another executable to disk.
func readMember(archivePath, assetName, member string, consume func(io.Reader, int64) error) error {
	if strings.HasSuffix(assetName, ".zip") {
		return extractZip(archivePath, assetName, member, consume)
	}
	return extractTarGz(archivePath, assetName, member, consume)
}

func extractZip(archivePath, assetName, member string, consume func(io.Reader, int64) error) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", assetName, err)
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		if !f.Mode().IsRegular() || path.Base(f.Name) != member {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("read %s: %w", assetName, err)
		}
		err = consume(rc, int64(f.UncompressedSize64)) // #nosec G115 -- a size from a checksum-verified archive
		_ = rc.Close()
		return err
	}
	return fmt.Errorf("archive %s contains no %s executable", assetName, member)
}

func extractTarGz(archivePath, assetName, member string, consume func(io.Reader, int64) error) error {
	f, err := os.Open(archivePath) // #nosec G304 -- forge's own staged download
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", assetName, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", assetName, err)
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != member {
			continue
		}
		return consume(tr, hdr.Size)
	}
	return fmt.Errorf("archive %s contains no %s executable", assetName, member)
}

func writeExecutable(src io.Reader, size int64, member, dest string) error {
	if size < 0 || size > maxArchiveBytes {
		return fmt.Errorf("extract %s: entry size %d is out of range", member, size)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755) // #nosec G302 G304 -- an executable in forge's staging dir
	if err != nil {
		return err
	}
	n, err := io.CopyN(out, src, size)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("extract %s: %w", member, err)
	}
	if n != size {
		return fmt.Errorf("extract %s: wrote %d of %d bytes", member, n, size)
	}
	// The umask may have stripped bits from OpenFile's mode; the cache is
	// shared, so make the mode exact.
	return os.Chmod(dest, 0o755) // #nosec G302 -- an executable must be executable
}
