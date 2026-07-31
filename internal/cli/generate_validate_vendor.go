package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/assets"
)

// ensureValidateProtoVendored vendors protovalidate's option-definition
// file into proto/buf/validate/validate.proto WHEN — and only when — some
// project proto imports it (`import "buf/validate/validate.proto";`).
//
// This is the offline-first analogue of how forge.proto is vendored: a
// project that uses `[(buf.validate.field)...]` rules needs the extension
// definitions on disk for buf to compile, and a project that doesn't
// stays clean (no 5k-line file it never asked for). It is idempotent and
// runs before every buf invocation, so it also self-heals an existing
// project the moment a user adds the import. project.go (the one-time
// scaffolder) is deliberately untouched — vendoring is demand-driven at
// generate time, not stamped at birth.
func ensureValidateProtoVendored(projectDir string) error {
	protoRoot := filepath.Join(projectDir, "proto")
	uses, err := protosImportValidate(protoRoot)
	if err != nil || !uses {
		return err
	}
	dest := filepath.Join(projectDir, filepath.FromSlash(assets.ValidateProtoVendorRelPath))
	if _, statErr := os.Stat(dest); statErr == nil {
		return nil // already vendored
	}
	return assets.WriteValidateProto(filepath.Dir(dest))
}

// protosImportValidate reports whether any .proto under root (excluding
// the vendored copy itself) imports buf/validate/validate.proto.
func protosImportValidate(root string) (bool, error) {
	needle := []byte(`"` + assets.ValidateProtoImportPath + `"`)
	found := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".proto") {
			return nil
		}
		// Skip the vendored tree so the file's own text can't self-trigger.
		if strings.Contains(filepath.ToSlash(path), "/proto/buf/validate/") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(b, needle) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(err, fs.SkipAll) {
		err = nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return found, err
}
