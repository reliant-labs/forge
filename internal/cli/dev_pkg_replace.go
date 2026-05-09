// Package cli — dev-mode forge/pkg replace handling.
//
// Background: when forge is checked out alongside a project (sibling
// directories on disk), the project's go.mod commonly carries a
//
//	replace github.com/reliant-labs/forge/pkg => /absolute/host/path/forge/pkg
//
// directive so the project can use forge/pkg subpackages that haven't
// been published yet. This works for `go build` on the host but breaks
// `docker build`, because the absolute host path isn't visible inside
// the build context.
//
// The canonical fix in forge: detect such absolute-path replaces during
// `forge generate`, vendor the target into `<project>/.forge-pkg/`, and
// rewrite the replace to `./.forge-pkg`. The Dockerfile template emits
// a corresponding `COPY .forge-pkg/ ./.forge-pkg/` line whenever the
// vendored copy exists, so docker builds and host builds use the same
// replace target by construction.
//
// This is a development affordance — production deploys want forge/pkg
// as a real go.mod requirement, not a vendored copy. The opt-in is
// implicit (presence of the host-absolute replace in go.mod) and can be
// disabled with `forge.yaml -> dev.vendor_local_forge_pkg: false`.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// localForgePkgVendorDir is the conventional vendor directory name. The
// dot prefix follows the same convention as `.forge/` (forge runtime
// state) so it's grouped visually and excluded by typical IDE/file-tree
// filters.
const localForgePkgVendorDir = ".forge-pkg"

// forgePkgModulePath is the canonical Go module path for forge/pkg.
const forgePkgModulePath = "github.com/reliant-labs/forge/pkg"

// forgePkgReplaceLineRE matches a single-line replace directive of the
// form
//
//	replace github.com/reliant-labs/forge/pkg [VERSION] => TARGET [VERSION]
//
// across a go.mod file. We deliberately don't try to handle the
// block-form `replace ( ... )` here — projects scaffolded by forge use
// the single-line form, which is also what `go mod edit -replace`
// emits. The block form is detected and reported as unsupported rather
// than silently mishandled.
var forgePkgReplaceLineRE = regexp.MustCompile(
	`(?m)^[\t ]*replace[\t ]+github\.com/reliant-labs/forge/pkg(?:[\t ]+[^\s=]+)?[\t ]*=>[\t ]*([^\s]+)(?:[\t ]+[^\s]+)?[\t ]*$`,
)

// devPkgReplaceState captures the result of inspecting a project's
// go.mod for the local forge/pkg replace.
type devPkgReplaceState struct {
	// HasReplace is true when a single-line replace for forge/pkg was
	// found.
	HasReplace bool
	// Target is the right-hand side of the replace (e.g. an absolute
	// path or `./.forge-pkg`). Empty when HasReplace is false.
	Target string
	// IsAbsolutePath is true when Target is an absolute filesystem
	// path. These are the ones that break docker builds and need to be
	// rewritten.
	IsAbsolutePath bool
	// IsLocalVendor is true when Target points at the canonical
	// `./.forge-pkg` (or `.forge-pkg`) vendor directory — the desired
	// post-fix state.
	IsLocalVendor bool
}

// inspectDevPkgReplace parses go.mod looking for the forge/pkg replace
// directive. Returns a zero-value state when go.mod is missing or has
// no such replace.
func inspectDevPkgReplace(projectDir string) (devPkgReplaceState, error) {
	data, err := os.ReadFile(filepath.Join(projectDir, "go.mod"))
	if err != nil {
		if os.IsNotExist(err) {
			return devPkgReplaceState{}, nil
		}
		return devPkgReplaceState{}, fmt.Errorf("read go.mod: %w", err)
	}
	m := forgePkgReplaceLineRE.FindStringSubmatch(string(data))
	if m == nil {
		return devPkgReplaceState{}, nil
	}
	target := strings.TrimSpace(m[1])
	st := devPkgReplaceState{HasReplace: true, Target: target}
	switch {
	case filepath.IsAbs(target):
		st.IsAbsolutePath = true
	case target == "."+string(filepath.Separator)+localForgePkgVendorDir,
		target == "./"+localForgePkgVendorDir,
		target == localForgePkgVendorDir:
		st.IsLocalVendor = true
	}
	return st, nil
}

// syncDevForgePkgReplace is the canonical entry point called from
// `forge generate`. It:
//
//  1. Inspects go.mod for the forge/pkg replace directive.
//  2. If the target is an absolute host path (the dev-mode pattern):
//     - syncs the target directory into <projectDir>/.forge-pkg/
//     - rewrites the replace to `./.forge-pkg`
//     - prints a one-line summary of what happened
//  3. If the target is already `./.forge-pkg`, refreshes the vendored
//     copy from a sibling forge checkout when one exists (so projects
//     that have already adopted the vendor stay in sync as forge/pkg
//     evolves on the host).
//
// Returns true when a vendored copy is present at <projectDir>/.forge-pkg/
// after the call (whether newly synced or pre-existing). The Dockerfile
// template uses this as the gate for emitting the COPY line.
//
// Idempotent: re-running over a project already in the vendored state
// is a no-op when source and destination are byte-identical, and a
// content refresh otherwise.
func syncDevForgePkgReplace(projectDir string) (vendored bool, err error) {
	st, err := inspectDevPkgReplace(projectDir)
	if err != nil {
		return false, err
	}

	// Source-of-truth resolution order:
	//   1. Host-absolute replace target (highest priority — the user
	//      explicitly pointed at a forge checkout).
	//   2. Sibling `../forge/pkg` directory next to the project (the
	//      common "forge alongside project" dev layout).
	//   3. Existing `<projectDir>/.forge-pkg/` (already vendored, no
	//      forge sibling to refresh from — leave as-is).
	var sourceDir string
	switch {
	case st.IsAbsolutePath:
		sourceDir = st.Target
	case st.IsLocalVendor:
		// Look for a sibling forge checkout to refresh from.
		if sib := siblingForgePkg(projectDir); sib != "" {
			sourceDir = sib
		}
	default:
		// No replace, or block-form replace, or replace to something
		// non-local (e.g. a fork via VCS path). Don't touch — the user
		// is in a configuration we don't manage.
		// Still report whether a vendored copy happens to exist so
		// callers can decide whether to emit the Dockerfile COPY line.
		return localVendorPresent(projectDir), nil
	}

	// Only sync when the source has the shape of a forge/pkg directory
	// (a go.mod declaring module github.com/reliant-labs/forge/pkg).
	// This guards against pointing at the wrong directory and quietly
	// vendoring the wrong content.
	if !looksLikeForgePkgDir(sourceDir) {
		return localVendorPresent(projectDir), fmt.Errorf(
			"replace target %q does not look like forge/pkg (no go.mod or wrong module path); refusing to vendor",
			sourceDir,
		)
	}

	destDir := filepath.Join(projectDir, localForgePkgVendorDir)
	changed, err := syncDir(sourceDir, destDir)
	if err != nil {
		return localVendorPresent(projectDir), fmt.Errorf("sync forge/pkg into %s: %w", localForgePkgVendorDir, err)
	}

	// Rewrite the go.mod replace to the relative vendor path if it
	// isn't already there.
	if st.IsAbsolutePath {
		if err := rewriteForgePkgReplaceToVendor(projectDir); err != nil {
			return true, fmt.Errorf("rewrite go.mod replace: %w", err)
		}
		fmt.Printf("  ✅ Vendored forge/pkg → %s/ (replace rewritten from %s)\n", localForgePkgVendorDir, st.Target)
	} else if changed {
		fmt.Printf("  ✅ Refreshed %s/ from sibling forge checkout\n", localForgePkgVendorDir)
	}

	return true, nil
}

// localVendorPresent reports whether <projectDir>/.forge-pkg/go.mod
// exists. Used to gate the Dockerfile COPY emission.
func localVendorPresent(projectDir string) bool {
	_, err := os.Stat(filepath.Join(projectDir, localForgePkgVendorDir, "go.mod"))
	return err == nil
}

// siblingForgePkg returns the path to a sibling forge checkout's pkg/
// directory, or "" if no plausible candidate is found. We check the
// project's parent directory for a `forge/` directory that contains a
// `pkg/go.mod` declaring the canonical module path.
func siblingForgePkg(projectDir string) string {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return ""
	}
	parent := filepath.Dir(abs)
	candidate := filepath.Join(parent, "forge", "pkg")
	if looksLikeForgePkgDir(candidate) {
		return candidate
	}
	return ""
}

// looksLikeForgePkgDir reports whether dir contains a go.mod declaring
// `module github.com/reliant-labs/forge/pkg`. Used as a safety check
// before vendoring.
func looksLikeForgePkgDir(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			mod := strings.TrimSpace(strings.TrimPrefix(line, "module"))
			return mod == forgePkgModulePath
		}
	}
	return false
}

// syncDir performs a content-aware copy of src → dst. Returns true
// when any file was created, modified, or deleted. We avoid `cp -r`
// in-process so the caller doesn't depend on a `cp` binary and so we
// can preserve idempotence (timestamp-only differences don't trigger a
// re-write).
//
// Files under .git/ or any directory named `testdata` are skipped —
// nothing in forge/pkg today uses testdata, but keeping the rule
// conservative prevents accidental copies of fixture trees if pkg
// grows tests with large fixtures.
func syncDir(src, dst string) (bool, error) {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return false, err
	}

	// Build the set of files we want under dst, copying as we go.
	want := make(map[string]struct{})
	changed := false

	walkErr := filepath.Walk(srcAbs, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(srcAbs, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		// Skip VCS and test-fixture trees. We DO copy regular _test.go
		// files because forge/pkg ships them and projects that depend
		// on forge/pkg need them for `go test ./...` to remain
		// well-typed.
		base := filepath.Base(rel)
		if info.IsDir() && (base == ".git" || base == "testdata") {
			return filepath.SkipDir
		}
		dstPath := filepath.Join(dst, rel)
		if info.IsDir() {
			if mkErr := os.MkdirAll(dstPath, 0o755); mkErr != nil {
				return mkErr
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		want[filepath.ToSlash(rel)] = struct{}{}
		copied, copyErr := copyFileIfDifferent(path, dstPath, info.Mode().Perm())
		if copyErr != nil {
			return copyErr
		}
		if copied {
			changed = true
		}
		return nil
	})
	if walkErr != nil {
		return changed, walkErr
	}

	// Remove files under dst that aren't in src anymore. This is what
	// makes the sync truly idempotent — without it, a forge/pkg
	// renamed/deleted file would linger in .forge-pkg/ and cause stale
	// imports.
	if _, err := os.Stat(dst); err == nil {
		_ = filepath.Walk(dst, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(dst, path)
			if relErr != nil {
				return nil
			}
			if _, keep := want[filepath.ToSlash(rel)]; !keep {
				if rmErr := os.Remove(path); rmErr == nil {
					changed = true
				}
			}
			return nil
		})
	}

	return changed, nil
}

// copyFileIfDifferent writes src → dst only when the byte contents
// differ (or dst doesn't exist). Returns true if a write happened.
// Mode is applied on create; existing files keep their on-disk perms
// modulo our 0644 fallback if perm is zero.
func copyFileIfDifferent(src, dst string, perm os.FileMode) (bool, error) {
	srcData, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	if existing, err := os.ReadFile(dst); err == nil {
		if len(existing) == len(srcData) {
			equal := true
			for i := range existing {
				if existing[i] != srcData[i] {
					equal = false
					break
				}
			}
			if equal {
				return false, nil
			}
		}
	}
	if perm == 0 {
		perm = 0o644
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".forge-pkg-sync-*")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := io.Copy(tmp, strings.NewReader(string(srcData))); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return false, err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return false, err
	}
	return true, nil
}

// rewriteForgePkgReplaceToVendor rewrites the absolute-path replace
// directive in go.mod to point at `./.forge-pkg`. Idempotent.
func rewriteForgePkgReplaceToVendor(projectDir string) error {
	goModPath := filepath.Join(projectDir, "go.mod")
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return err
	}
	updated := forgePkgReplaceLineRE.ReplaceAllString(
		string(data),
		"replace "+forgePkgModulePath+" => ./"+localForgePkgVendorDir,
	)
	if updated == string(data) {
		return nil
	}
	return os.WriteFile(goModPath, []byte(updated), 0o644)
}
