// Package kclvendor materializes the forge KCL module into generated
// projects and points their kcl.mod at that copy.
//
// A scaffolded project's `deploy/kcl/kcl.mod` depends on the `forge` KCL
// module — the typed schemas + render layer its env `main.k` files
// import. Forge resolves that dependency ONE way, on every build of
// forge: it materializes the module EMBEDDED IN THE BINARY
// (github.com/reliant-labs/forge/kcl) into `<project>/.forge-kcl/` and
// writes a RELATIVE path dependency. Relative, not absolute: the
// vendored copy travels with the repo, so containers, CI checkouts, and
// other machines resolve it identically.
//
// There is deliberately no second mechanism. Forge previously scaffolded
// a published `kcl-vX.Y.Z` git tag on release builds and vendored only
// on dev builds, which failed in three separate ways: the tag was never
// published so every released scaffold was unresolvable; a release build
// DELETED a working `.forge-kcl/` and rewrote the dependency back to the
// dead tag; and even a correctly published tag would have required
// network plus git auth at render time, which an air-gapped or
// offline-CI render does not have. Resolving from the binary's own copy
// removes all three, and removes a release step that has to be
// remembered. See docs/adr/0001-always-vendor-forge-kcl.md.
//
// The cost of vendoring is staleness: the module refreshes when `forge
// generate` runs, so a project can sit on a copy an older forge wrote.
// Materialize therefore stamps the materializing forge's version into
// the vendor dir, and [Stale] reports a mismatch so the render seam can
// say so out loud rather than let a confusing schema error stand in for
// "you have not regenerated".
//
// Staleness has a second, sharper failure mode that the stamp did not
// originally guard: refreshing BACKWARDS. `forge generate` rewrites the
// vendor dir from whatever binary happens to be on PATH, so a developer
// or CI job running a slightly older forge silently replaced a project's
// KCL with an OLDER schema. That is not hypothetical — it broke prod.
// An agent ran `forge generate` in control-plane with a binary predating
// forge d51e8b6c; generate overwrote `.forge-kcl/schema.k` with the stale
// copy, including an outdated Gateway listener rule, and prod's `env
// render` started failing. Nothing warned: the stamp recorded the
// downgrade as cheerfully as it records an upgrade, so the marker was a
// no-op that looked like a guard. The symptom surfaced later, in a
// different command, looking like a different bug.
//
// So Materialize now REFUSES to write an older module over a newer one
// ([DowngradeError]), and refuses loudly and early — before any byte is
// written — rather than producing a subtly-wrong render downstream. The
// refusal is precise in both directions: it fires only when the running
// forge is provably older by semver AND the embedded module would
// actually change bytes on disk, so identical-content republishes and
// unorderable version strings never wedge a project.
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

	"golang.org/x/mod/semver"

	"github.com/reliant-labs/forge/internal/buildinfo"
	forgekcl "github.com/reliant-labs/forge/kcl"
)

// VendorDirName is the project-root directory the embedded module is
// materialized into. The dot prefix groups it with `.forge/` and
// `.forge-pkg/` (forge-maintained state).
const VendorDirName = ".forge-kcl"

// StampFileName is the file Materialize writes inside the vendor dir
// recording which forge version produced the copy. A dotfile so KCL
// never treats it as a source, and read back by [Stale] to tell a
// project its vendored module predates the forge now rendering it.
const StampFileName = ".forge-version"

// MarkerHeader is the first line of the forge-maintained dependency
// block — the ownership anchor, mirroring the Dockerfile vendor COPY
// block's header.
const MarkerHeader = "# ── Vendored forge KCL module (maintained by forge generate) ──"

// legacyMarkerHeaders are marker headers earlier forge versions wrote.
// ownedBlockStart accepts them so an upgrade rewrites the whole stale
// block instead of stacking a new one above the old comments.
var legacyMarkerHeaders = []string{
	"# ── Dev-mode local forge KCL module vendor ──",
}

// markerBody is the explanatory comment between the header and the
// dependency line.
const markerBody = `#
# ` + "`forge generate`" + ` materializes the KCL module embedded in the forge
# binary into ` + "`" + VendorDirName + "/`" + ` at the project root and points this
# dependency at it by RELATIVE path. That copy travels with the repo, so
# containers, CI checkouts and other machines resolve the identical
# module — with no network, no git auth, and nothing to publish.
#
# Commit ` + "`" + VendorDirName + "/`" + `. It refreshes on every ` + "`forge generate`" + `.`

// DepKind classifies the forge dependency line found in a kcl.mod.
type DepKind int

const (
	// DepNone — the file has no `forge = …` dependency line.
	DepNone DepKind = iota
	// DepGitTag — `forge = { git = "…", tag = "…" }`. The shape older
	// scaffolds emitted; forge rewrites it to the vendored path.
	DepGitTag
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

// gitTagRHSRE recognizes the git+tag inline-table shape older scaffolds
// emitted, which EnsureVendorDep rewrites to the vendored path.
var gitTagRHSRE = regexp.MustCompile(`^\{\s*git\s*=\s*"[^"]+"\s*,\s*tag\s*=\s*"[^"]+"\s*\}$`)

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
		case gitTagRHSRE.MatchString(rhs):
			kind = DepGitTag
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
// comment run directly above it. If that run begins with MarkerHeader —
// or a header an earlier forge wrote — the block [start..depIdx] is
// forge-owned and returns start; otherwise returns depIdx (only the line
// itself is ours to touch, so user comments above it are preserved).
func ownedBlockStart(lines []string, depIdx int) int {
	start := depIdx
	for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "#") {
		start--
	}
	if start == depIdx {
		return depIdx
	}
	head := strings.TrimSpace(lines[start])
	if head == MarkerHeader {
		return start
	}
	for _, legacy := range legacyMarkerHeaders {
		if head == legacy {
			return start
		}
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
			"%s has no `forge = …` dependency line — cannot point it at the vendored module; add `%s` by hand",
			kclModPath, vendorDepLine(relPath))}, nil
	case DepUnrecognized:
		return Result{Warning: fmt.Sprintf(
			"%s carries a forge dependency in a shape `forge generate` does not manage — leaving it untouched; expected `%s` for the vendored module",
			kclModPath, vendorDepLine(relPath))}, nil
	}

	// DepGitTag, DepAbsolutePath, or DepVendored (possibly with a
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

// DowngradeError is returned by [Materialize] when the running forge's
// embedded KCL module would overwrite a vendor dir that a NEWER forge
// wrote. It carries both versions so callers can print the two identities
// side by side — "which binary is doing this" is the one question the
// original silent-overwrite failure left unanswerable.
type DowngradeError struct {
	// Stamped is the version recorded in .forge-kcl/.forge-version —
	// the forge that produced the copy currently on disk.
	Stamped string
	// Running is the version of the forge that just tried to overwrite it.
	Running string
	// ProjectDir is the project whose vendor dir was protected.
	ProjectDir string
}

func (e *DowngradeError) Error() string {
	upgrade := "go install github.com/reliant-labs/forge/cmd/forge@" + e.Stamped
	if buildinfo.IsDevVersion(e.Stamped) {
		// A `+dirty`/workspace stamp names no ref any proxy can serve,
		// so pointing at it would hand the user a command that fails.
		upgrade = "rebuild forge from a checkout at or after " + e.Stamped + " (task install:dev)"
	}
	return fmt.Sprintf(
		"refusing to overwrite %s/ with an OLDER forge's KCL module.\n"+
			"    on disk:  %s  (wrote %s/)\n"+
			"    running:  %s  (this binary)\n"+
			"  This forge is older than the one that vendored the module, so refreshing would\n"+
			"  REPLACE the project's KCL schemas with a stale copy. That exact downgrade shipped\n"+
			"  an outdated Gateway listener rule into control-plane and broke prod's `env render`,\n"+
			"  with the failure surfacing later in a different command.\n"+
			"  Fix: upgrade this binary — %s\n"+
			"  Or, if the downgrade is deliberate (rolling forge back on purpose):\n"+
			"    forge generate --allow-kcl-downgrade",
		VendorDirName, e.Stamped, VendorDirName, e.Running, upgrade)
}

// checkDowngrade returns a [DowngradeError] when the running forge is
// provably older, by semver, than the forge stamped on the vendor dir.
//
// Deliberately narrow — every branch that returns nil is a case where
// refusing would wedge a project for no safety gain:
//
//   - no vendor dir, or no stamp: nothing to protect (and an unstamped
//     copy predates stamping entirely, so a refresh is the whole point).
//   - either version unorderable by semver (the "dev" sentinel,
//     "(devel)", a hand-edited stamp): comparison would be a coin flip,
//     and a guard that fires on a coin flip gets disabled by everyone.
//   - equal or newer: the normal refresh path.
//
// Build metadata is stripped before comparing: `v0.1.12+dirty` and
// `v0.1.12` are the same source vintage, and semver.Compare already
// ignores it — but +dev/+dirty floors compare EQUAL to the release tag,
// which is what we want (a dirty local build of the same release must
// not be called a downgrade).
func checkDowngrade(projectDir string) error {
	if !Present(projectDir) {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(projectDir, VendorDirName, StampFileName))
	if err != nil {
		return nil
	}
	stamped := strings.TrimSpace(string(data))
	running := buildinfo.Version()
	if stamped == "" || stamped == running {
		return nil
	}
	if !semver.IsValid(stamped) || !semver.IsValid(running) {
		return nil
	}
	if semver.Compare(running, stamped) >= 0 {
		return nil
	}
	return &DowngradeError{Stamped: stamped, Running: running, ProjectDir: projectDir}
}

// wouldChange reports whether materializing the embedded module into dst
// would create, rewrite, or delete anything.
//
// The downgrade guard consults this so it fires on SUBSTANCE, not on
// version strings alone: an older forge whose embedded module is
// byte-identical to what is already vendored is doing nothing, and
// blocking it would break `forge generate` for anyone on a pinned older
// build with no actual schema difference. Only a downgrade that would
// really move bytes is worth refusing.
func wouldChange(dst string, want map[string]struct{}) (bool, error) {
	for path := range want {
		src, err := fs.ReadFile(forgekcl.Module, path)
		if err != nil {
			return false, err
		}
		existing, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(path)))
		if err != nil || !bytes.Equal(existing, src) {
			return true, nil
		}
	}
	stray := false
	_ = filepath.Walk(dst, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || stray {
			return nil
		}
		rel, relErr := filepath.Rel(dst, path)
		if relErr != nil {
			return nil
		}
		slashRel := filepath.ToSlash(rel)
		if slashRel == "kcl.mod.lock" || slashRel == StampFileName {
			return nil
		}
		if _, keep := want[slashRel]; !keep {
			stray = true
		}
		return nil
	})
	return stray, nil
}

// embeddedModuleFiles lists every file path in the embedded KCL module,
// in the forward-slash form both the walk and the stray sweep key on.
func embeddedModuleFiles() (map[string]struct{}, error) {
	want := make(map[string]struct{})
	err := fs.WalkDir(forgekcl.Module, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != "." && !d.IsDir() {
			want[path] = struct{}{}
		}
		return nil
	})
	return want, err
}

// Materialize syncs the embedded forge KCL module into
// <projectDir>/.forge-kcl. Content-hash idempotent: byte-identical
// files are not rewritten, files that drifted are replaced, and files
// under the vendor dir that are not in the embedded module are deleted
// (so renames/removals upstream never leave stale sources behind).
// Returns true when anything was created, rewritten, or deleted.
//
// It refuses, before writing anything, when the running forge is older
// than the forge that vendored the copy on disk AND the refresh would
// actually change bytes — see [DowngradeError] and the package doc's
// account of the prod render this broke. allowDowngrade is the deliberate
// opt-out (`forge generate --allow-kcl-downgrade`), for rolling forge
// back on purpose.
func Materialize(projectDir string, allowDowngrade bool) (changed bool, err error) {
	dst := filepath.Join(projectDir, VendorDirName)
	want, err := embeddedModuleFiles()
	if err != nil {
		return false, fmt.Errorf("read embedded forge KCL module: %w", err)
	}

	// Guard FIRST, and on a would-change check rather than the version
	// alone. Ordering is the point: a guard that runs after the walk has
	// already rewritten half the files protects nothing.
	if !allowDowngrade {
		if dErr := checkDowngrade(projectDir); dErr != nil {
			differs, cErr := wouldChange(dst, want)
			if cErr != nil {
				return false, cErr
			}
			if differs {
				return false, dErr
			}
		}
	}

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

	// Delete strays. Two files are tolerated rather than treated as
	// strays: kcl.mod.lock, which kpm derives inside the vendor dir on
	// some resolution paths, and the version stamp this function writes
	// below — deleting either every run would churn.
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
			if slashRel == "kcl.mod.lock" || slashRel == StampFileName {
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
	// Stamp the materializing forge's version LAST, so a stamp is only
	// ever present over a complete copy. This is what makes vendoring
	// safe to rely on as the single mechanism: the module refreshes on
	// `forge generate` and nowhere else, so without a recorded version a
	// project could sit on an old copy and the only symptom would be a
	// schema error that names the wrong cause.
	stampPath := filepath.Join(dst, StampFileName)
	stamp := []byte(buildinfo.Version() + "\n")
	if existing, rerr := os.ReadFile(stampPath); rerr != nil || !bytes.Equal(existing, stamp) {
		if werr := os.WriteFile(stampPath, stamp, 0o644); werr != nil {
			return changed, fmt.Errorf("write %s: %w", StampFileName, werr)
		}
		changed = true
	}
	return changed, nil
}

// Stale reports whether <projectDir>/.forge-kcl was materialized by a
// DIFFERENT forge version than the one running now, returning the
// recorded version for the message. A vendor dir that is absent, or one
// whose stamp matches, is not stale.
//
// An unstamped copy (materialized by a forge predating the stamp) counts
// as stale with an empty version: it genuinely was written by another
// forge, and one `forge generate` clears it for good.
func Stale(projectDir string) (stale bool, stampedVersion string) {
	if !Present(projectDir) {
		return false, ""
	}
	data, err := os.ReadFile(filepath.Join(projectDir, VendorDirName, StampFileName))
	if err != nil {
		return true, ""
	}
	got := strings.TrimSpace(string(data))
	return got != buildinfo.Version(), got
}
