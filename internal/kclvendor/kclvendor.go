// Package kclvendor implements the dev-mode vendoring of the forge KCL
// module into generated projects — the KCL sibling of the `.forge-pkg`
// Go-module flow in internal/cli/dev_pkg_replace.go.
//
// Background: a scaffolded project's `deploy/kcl/kcl.mod` depends on the
// `forge` KCL module (typed schemas + render layer) via a published
// `kcl-vX.Y.Z` git tag. Dev builds of forge have no published tag — the
// same way they have no published forge/pkg Go module version — so on
// dev builds forge materializes the module EMBEDDED IN THE BINARY
// (github.com/reliant-labs/forge/kcl) into `<project>/.forge-kcl/` and
// rewrites the kcl.mod dependency to a relative path. Relative, not
// absolute: the vendored copy travels with the repo, so containers, CI
// checkouts, and other machines resolve it identically.
//
// Release builds scaffold the published tag (unchanged behavior) and
// restore it — removing `.forge-kcl/` — when they encounter a project a
// dev build previously vendored, mirroring the `.forge-pkg` → published
// pin swap semantics.
//
// The kcl.mod is user-owned. All edits are marker-delimited and
// exact-match (the same discipline as the Dockerfile COPY block in
// internal/cli/generate_pipeline.go): only a recognized `forge = { … }`
// dependency line is ever rewritten, and a hand-rewritten file the
// patcher cannot prove it understands is left alone with a warning for
// the caller to surface.
package kclvendor

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	forgekcl "github.com/reliant-labs/forge/kcl"
)

// VendorDirName is the project-root directory the embedded module is
// materialized into. The dot prefix groups it with `.forge/` and
// `.forge-pkg/` (forge-maintained state).
const VendorDirName = ".forge-kcl"

// PublishedGitURL and PublishedTag pin the published registry reference
// the scaffold emits on release builds and the restore path re-emits
// when un-vendoring. PublishedDepLine must stay in sync with
// internal/templates/deploy/kcl/kcl.mod.tmpl (asserted by unit test).
const (
	PublishedGitURL = "https://github.com/reliant-labs/forge.git"
	PublishedTag    = "kcl-v0.1.0"
)

// PublishedDepLine is the exact dependency line for the published module.
var PublishedDepLine = fmt.Sprintf("forge = { git = %q, tag = %q }", PublishedGitURL, PublishedTag)

// MarkerHeader is the first line of the forge-maintained dependency
// block — the ownership/removal anchor, mirroring the Dockerfile
// dev-vendor COPY block's header.
const MarkerHeader = "# ── Dev-mode local forge KCL module vendor ──"

// markerBody is the explanatory comment between the header and the
// dependency line.
const markerBody = `#
# The published ` + "`kcl-vX.Y.Z`" + ` tag is not resolvable for a dev build of
# forge, so ` + "`forge generate`" + ` materializes the module embedded in the
# forge binary into ` + "`" + VendorDirName + "/`" + ` at the project root and points this
# dependency at it via a RELATIVE path — containers and CI resolve the
# same copy because it travels with the repo.
#
# This block is maintained by ` + "`forge generate`" + `: refreshed while the
# project is built with a dev forge, restored to the published tag by
# release builds.`

// DepKind classifies the forge dependency line found in a kcl.mod.
type DepKind int

const (
	// DepNone — the file has no `forge = …` dependency line.
	DepNone DepKind = iota
	// DepPublished — `forge = { git = "…", tag = "…" }`.
	DepPublished
	// DepAbsolutePath — `forge = { path = "/abs/host/path" }` (the
	// hand-patch pattern this package exists to replace).
	DepAbsolutePath
	// DepVendored — `forge = { path = "…/.forge-kcl" }` (any relative
	// spelling that targets the vendor dir).
	DepVendored
	// DepUnrecognized — a forge line (or lines) exists in a shape the
	// patcher does not manage: multiple lines, a TOML table, extra
	// keys, a path to somewhere that is not the vendor dir, etc.
	DepUnrecognized
)

// Result reports what a patch call did.
type Result struct {
	// Changed is true when the file was rewritten.
	Changed bool
	// Warning, when non-empty, is a caller-surfaceable reason the file
	// was left alone (hand-rewritten beyond recognition, …).
	Warning string
}

// forgeDepLineRE matches a single-line forge dependency in kcl.mod.
// The RHS is parsed separately; this only anchors the line.
var forgeDepLineRE = regexp.MustCompile(`(?m)^[\t ]*forge[\t ]*=[\t ]*(.+?)[\t ]*$`)

// publishedRHSRE recognizes the published git+tag inline-table shape.
var publishedRHSRE = regexp.MustCompile(`^\{\s*git\s*=\s*"[^"]+"\s*,\s*tag\s*=\s*"[^"]+"\s*\}$`)

// pathRHSRE recognizes the local-path inline-table shape and captures
// the path.
var pathRHSRE = regexp.MustCompile(`^\{\s*path\s*=\s*"([^"]+)"\s*\}$`)

// classifyDep inspects file content and returns the dep kind plus the
// line index of the forge dependency (-1 when absent) and, for path
// deps, the target path.
func classifyDep(lines []string) (kind DepKind, depIdx int, pathTarget string) {
	depIdx = -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "forge") {
			continue
		}
		m := forgeDepLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if depIdx != -1 {
			return DepUnrecognized, depIdx, "" // multiple forge lines
		}
		depIdx = i
		rhs := strings.TrimSpace(m[1])
		switch {
		case publishedRHSRE.MatchString(rhs):
			kind = DepPublished
		case pathRHSRE.MatchString(rhs):
			target := pathRHSRE.FindStringSubmatch(rhs)[1]
			pathTarget = target
			if filepath.IsAbs(target) {
				kind = DepAbsolutePath
			} else if filepath.Base(filepath.FromSlash(target)) == VendorDirName {
				kind = DepVendored
			} else {
				kind = DepUnrecognized // relative path to something else (e.g. "../forge/kcl")
			}
		default:
			kind = DepUnrecognized
		}
	}
	if depIdx == -1 {
		return DepNone, -1, ""
	}
	return kind, depIdx, pathTarget
}

// ownedBlockStart walks up from the dependency line over the contiguous
// comment run directly above it. If that run begins with MarkerHeader,
// the block [start..depIdx] is forge-owned and returns start; otherwise
// returns depIdx (only the line itself is ours to touch — user comments
// above it are preserved).
func ownedBlockStart(lines []string, depIdx int) int {
	start := depIdx
	for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "#") {
		start--
	}
	if start < depIdx && strings.TrimSpace(lines[start]) == MarkerHeader {
		return start
	}
	return depIdx
}

// VendorDepPath returns the relative dependency path a kcl.mod at
// kclModPath must carry to reference <projectDir>/.forge-kcl, in the
// forward-slash form kcl.mod uses (e.g. "./.forge-kcl" from the project
// root, "../../.forge-kcl" from deploy/kcl/).
func VendorDepPath(kclModPath, projectDir string) (string, error) {
	rel, err := filepath.Rel(filepath.Dir(kclModPath), filepath.Join(projectDir, VendorDirName))
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if !strings.HasPrefix(rel, "../") {
		rel = "./" + rel
	}
	return rel, nil
}

// vendorDepLine renders the dependency line for a given relative path.
func vendorDepLine(relPath string) string {
	return fmt.Sprintf("forge = { path = %q }", relPath)
}

// EnsureVendorDep rewrites the forge dependency in the kcl.mod at
// kclModPath to the marker-delimited relative-path vendor block.
// Idempotent: byte-identical no-op when the block is already exactly in
// place. A missing file is a silent no-op (the project has no KCL at
// that location); an unmanageable dependency shape is a no-op with a
// Warning for the caller to surface.
func EnsureVendorDep(kclModPath, projectDir string) (Result, error) {
	data, err := os.ReadFile(kclModPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("read %s: %w", kclModPath, err)
	}
	relPath, err := VendorDepPath(kclModPath, projectDir)
	if err != nil {
		return Result{}, err
	}
	lines := strings.Split(string(data), "\n")
	kind, depIdx, _ := classifyDep(lines)

	switch kind {
	case DepNone:
		return Result{Warning: fmt.Sprintf(
			"%s has no `forge = …` dependency line — cannot point it at the dev-vendored module; add `%s` (or the published tag) by hand",
			kclModPath, vendorDepLine(relPath))}, nil
	case DepUnrecognized:
		return Result{Warning: fmt.Sprintf(
			"%s carries a forge dependency in a shape `forge generate` does not manage — leaving it untouched; expected `%s` for the dev-vendored module",
			kclModPath, vendorDepLine(relPath))}, nil
	}

	// DepPublished, DepAbsolutePath, or DepVendored (possibly with a
	// different spelling / missing marker): replace the owned span with
	// the canonical block.
	blockStart := ownedBlockStart(lines, depIdx)
	block := append(strings.Split(MarkerHeader+"\n"+markerBody, "\n"), vendorDepLine(relPath))
	updated := make([]string, 0, len(lines)+len(block))
	updated = append(updated, lines[:blockStart]...)
	updated = append(updated, block...)
	updated = append(updated, lines[depIdx+1:]...)
	out := strings.Join(updated, "\n")
	if out == string(data) {
		return Result{}, nil
	}
	if err := os.WriteFile(kclModPath, []byte(out), 0o644); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", kclModPath, err)
	}
	// The sibling lock pins the previous resolution; it is derived
	// state, so drop it on a source swap and let kpm rebuild it.
	_ = os.Remove(filepath.Join(filepath.Dir(kclModPath), "kcl.mod.lock"))
	return Result{Changed: true}, nil
}

// RestorePublishedDep swaps a forge-owned vendor block back to the
// published git-tag dependency line. Only the marker-delimited block
// (or a bare vendor-path line) is restored; hand-authored shapes —
// including absolute-path overrides — are never rewritten. Idempotent.
func RestorePublishedDep(kclModPath string) (Result, error) {
	data, err := os.ReadFile(kclModPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("read %s: %w", kclModPath, err)
	}
	lines := strings.Split(string(data), "\n")
	kind, depIdx, _ := classifyDep(lines)
	if kind != DepVendored {
		return Result{}, nil // published already, user-authored, or nothing — not ours to restore
	}
	blockStart := ownedBlockStart(lines, depIdx)
	updated := make([]string, 0, len(lines))
	updated = append(updated, lines[:blockStart]...)
	updated = append(updated, PublishedDepLine)
	updated = append(updated, lines[depIdx+1:]...)
	out := strings.Join(updated, "\n")
	if out == string(data) {
		return Result{}, nil
	}
	if err := os.WriteFile(kclModPath, []byte(out), 0o644); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", kclModPath, err)
	}
	_ = os.Remove(filepath.Join(filepath.Dir(kclModPath), "kcl.mod.lock"))
	return Result{Changed: true}, nil
}

// InspectDep reports the dependency kind carried by the kcl.mod at
// kclModPath (DepNone when the file does not exist).
func InspectDep(kclModPath string) (DepKind, error) {
	data, err := os.ReadFile(kclModPath)
	if err != nil {
		if os.IsNotExist(err) {
			return DepNone, nil
		}
		return DepNone, err
	}
	kind, _, _ := classifyDep(strings.Split(string(data), "\n"))
	return kind, nil
}

// Present reports whether a materialized vendor copy exists at
// <projectDir>/.forge-kcl (kcl.mod as the marker file, mirroring
// localVendorPresent for .forge-pkg).
func Present(projectDir string) bool {
	_, err := os.Stat(filepath.Join(projectDir, VendorDirName, "kcl.mod"))
	return err == nil
}

// Materialize syncs the embedded forge KCL module into
// <projectDir>/.forge-kcl. Content-hash idempotent: byte-identical
// files are not rewritten, files that drifted are replaced, and files
// under the vendor dir that are not in the embedded module are deleted
// (so renames/removals upstream never leave stale sources behind).
// Returns true when anything was created, rewritten, or deleted.
func Materialize(projectDir string) (changed bool, err error) {
	dst := filepath.Join(projectDir, VendorDirName)
	want := make(map[string]struct{})

	walkErr := fs.WalkDir(forgekcl.Module, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		want[path] = struct{}{}
		src, rerr := fs.ReadFile(forgekcl.Module, path)
		if rerr != nil {
			return rerr
		}
		if existing, eerr := os.ReadFile(target); eerr == nil && bytes.Equal(existing, src) {
			return nil
		}
		if merr := os.MkdirAll(filepath.Dir(target), 0o755); merr != nil {
			return merr
		}
		if werr := os.WriteFile(target, src, 0o644); werr != nil {
			return werr
		}
		changed = true
		return nil
	})
	if walkErr != nil {
		return changed, fmt.Errorf("materialize embedded forge KCL module into %s: %w", VendorDirName, walkErr)
	}

	// Delete strays. kcl.mod.lock is tolerated: kpm derives it inside
	// the vendor dir on some resolution paths, and deleting it every
	// run would churn.
	if _, err := os.Stat(dst); err == nil {
		_ = filepath.Walk(dst, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(dst, path)
			if relErr != nil {
				return nil
			}
			slashRel := filepath.ToSlash(rel)
			if slashRel == "kcl.mod.lock" {
				return nil
			}
			if _, keep := want[slashRel]; !keep {
				if rmErr := os.Remove(path); rmErr == nil {
					changed = true
				}
			}
			return nil
		})
	}
	return changed, nil
}

// Remove deletes the materialized vendor directory (release-build
// un-vendor path). Missing directory is a no-op.
func Remove(projectDir string) error {
	return os.RemoveAll(filepath.Join(projectDir, VendorDirName))
}
