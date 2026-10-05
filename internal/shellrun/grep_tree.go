package shellrun

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

func grepTree(root string, scan func(io.Reader, string)) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fh, oerr := os.Open(p)
		if oerr != nil {
			return nil
		}
		defer func() { _ = fh.Close() }()
		scan(fh, p)
		return nil
	})
}
