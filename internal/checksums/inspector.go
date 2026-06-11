// Ownership inspector — single source of truth for "what does forge own
// at relPath, and how should we treat it?".
//
// Background (2026-06-04 collapse pass): four parallel call sites in the
// generate pipeline each walked `FileChecksums.Files` and re-derived
// their own view of disowned / Tier-1 / Tier-2 / package-grouped layout:
//
//   - generate_pipeline.go: stepCheckTier1Drift (Tier-1 drift filter)
//   - generate_tier1_scope.go: filterTier1DriftInScope (owner-gate map)
//   - generate_dangling_check.go: disownedByDir + sibling enumeration
//   - generate_rename_check.go: snapshot of Tier-1 Go exports
//
// Each new check (the dangling-ref one was the immediate trigger)
// reinvented the manifest walk. The Inspector collapses those walks
// behind a small interface so the manifest shape is queried in exactly
// one place, the legacy Tier-0-as-Tier-1 promotion lives in exactly one
// helper, and AST-touching ownership queries (DeclaredTypesIn) sit
// alongside the existing ExtractGoExports helper.
//
// Scope: the Inspector answers questions about a *single project's*
// manifest plus on-disk content. It is not a long-lived service — call
// NewInspector once per pipeline run with the loaded *FileChecksums and
// project root, then use it. Per-directory and per-file results are
// memoized so repeated queries inside one pipeline run don't re-read
// the disk.
package checksums

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Inspector is the read-only query API over a FileChecksums manifest.
//
// All methods are safe to call concurrently after construction — the
// per-directory cache uses a mutex-free build-once pattern (cache maps
// are populated lazily but the underlying read is idempotent).
//
// Construction is intentionally cheap: NewInspector does not pre-walk
// the manifest. The first query that needs a grouped view populates
// the cache, subsequent queries reuse it.
type Inspector struct {
	root string
	cs   *FileChecksums

	// disownedGoByDir lazily groups disowned Go entries by their parent
	// directory. Populated on first call to DisownedGoFilesByDir or any
	// dependent query. nil means "not yet computed".
	disownedGoByDir map[string][]string

	// declaredTypesCache memoizes DeclaredTypesIn(relPath) results so
	// the dangling-ref check (which queries the same file multiple times
	// in some layouts) doesn't re-read or re-parse.
	declaredTypesCache map[string]map[string]bool

	// goSiblingsCache memoizes the list of project-relative Go file
	// paths under a directory. Keyed by the project-relative dir path.
	goSiblingsCache map[string][]string
}

// NewInspector constructs an Inspector over the given manifest. A nil
// cs is tolerated (every query returns a safe zero value) — keeps the
// helper callable from call sites that may be operating on a fresh
// project with no `.forge/checksums.json` yet.
func NewInspector(root string, cs *FileChecksums) *Inspector {
	return &Inspector{
		root:               root,
		cs:                 cs,
		declaredTypesCache: map[string]map[string]bool{},
		goSiblingsCache:    map[string][]string{},
	}
}

// entry returns the manifest entry for relPath plus a found flag. A nil
// manifest returns (zero, false). Centralizes the nil-guard so every
// other method on the inspector can assume a present manifest.
func (i *Inspector) entry(relPath string) (FileChecksumEntry, bool) {
	if i == nil || i.cs == nil || i.cs.Files == nil {
		return FileChecksumEntry{}, false
	}
	e, ok := i.cs.Files[relPath]
	return e, ok
}

// IsTracked reports whether forge has any record of relPath in the
// manifest. Untracked paths are user-owned by convention.
func (i *Inspector) IsTracked(relPath string) bool {
	_, ok := i.entry(relPath)
	return ok
}

// IsDisowned reports whether relPath was `forge disown`-ed: a one-way
// transfer to user ownership (Tier-2 with the disowned marker). Legacy
// fork-era entries (`forked: true`, pre-migration) answer true too —
// they are semantically the same "forge no longer regenerates this"
// state and convert to Disowned on the next pipeline run.
func (i *Inspector) IsDisowned(relPath string) bool {
	e, ok := i.entry(relPath)
	return ok && (e.Disowned || e.Forked)
}

// Tier returns the lifecycle tier of relPath: 1 (regenerated every
// run), 2 (one-shot scaffold), or 0 (untracked OR legacy entry whose
// tier field was never set). Callers that want Tier-0 promoted to
// Tier-1 (the historical default for pre-tier checksums) should use
// IsTier1.
func (i *Inspector) Tier(relPath string) int {
	e, ok := i.entry(relPath)
	if !ok {
		return 0
	}
	return e.Tier
}

// IsTier1 reports whether relPath is a Tier-1 (regenerated-every-run)
// file. Legacy Tier-0 entries are promoted to Tier-1 for back-compat —
// the same rule the pre-pipeline stomp guard uses.
func (i *Inspector) IsTier1(relPath string) bool {
	e, ok := i.entry(relPath)
	if !ok {
		return false
	}
	return e.Tier == 0 || e.Tier == 1
}

// IsTier2 reports whether relPath is a one-shot scaffold.
func (i *Inspector) IsTier2(relPath string) bool {
	return i.Tier(relPath) == 2
}

// IsGo is the small filename-suffix probe shared by every Go-aware
// ownership query. Replicated here (rather than re-exporting IsGoPath)
// so callers that have an *Inspector don't need a parallel import.
func (i *Inspector) IsGo(relPath string) bool {
	return IsGoPath(relPath)
}

// DisownedGoFilesByDir groups every disowned Go file in the manifest by
// its parent directory. The returned map's values are the
// project-relative paths of disowned entries in each directory, sorted
// for deterministic iteration. Legacy fork-era entries (`forked: true`)
// are included — same frozen-file semantics, pre-migration.
//
// Empty when no Go file is disowned. Memoized: subsequent calls return
// the same cached map.
func (i *Inspector) DisownedGoFilesByDir() map[string][]string {
	if i == nil || i.cs == nil {
		return map[string][]string{}
	}
	if i.disownedGoByDir != nil {
		return i.disownedGoByDir
	}
	out := map[string][]string{}
	for relPath, entry := range i.cs.Files {
		if !entry.Disowned && !entry.Forked {
			continue
		}
		if !IsGoPath(relPath) {
			continue
		}
		dir := filepath.Dir(relPath)
		out[dir] = append(out[dir], relPath)
	}
	for dir := range out {
		sort.Strings(out[dir])
	}
	i.disownedGoByDir = out
	return out
}

// Tier1GoFiles returns the sorted list of project-relative paths for
// every Tier-1 (including legacy Tier-0) Go file in the manifest.
// Used by the pre-codegen exports snapshot — rename detection only
// watches files forge still owns (disowned files are Tier-2 and fall
// out via the tier check; legacy forked entries are skipped explicitly
// until the pipeline migration converts them).
func (i *Inspector) Tier1GoFiles() []string {
	if i == nil || i.cs == nil {
		return nil
	}
	var out []string
	for relPath, entry := range i.cs.Files {
		if entry.Forked {
			continue
		}
		if entry.Tier != 0 && entry.Tier != 1 {
			continue
		}
		if !IsGoPath(relPath) {
			continue
		}
		out = append(out, relPath)
	}
	sort.Strings(out)
	return out
}

// GoSiblingsIn returns the project-relative paths of every *.go file
// (excluding *_test.go) physically present under projectRoot/relDir.
// Reads the directory once and memoizes the result.
//
// Returns an error only when the directory cannot be read. Individual
// non-Go files and test files are silently skipped — they don't
// contribute to package-local type resolution.
func (i *Inspector) GoSiblingsIn(relDir string) ([]string, error) {
	if cached, ok := i.goSiblingsCache[relDir]; ok {
		return cached, nil
	}
	absDir := filepath.Join(i.root, relDir)
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(relDir, name))
	}
	sort.Strings(out)
	i.goSiblingsCache[relDir] = out
	return out, nil
}

// DeclaredTypesIn returns the set of top-level type identifiers declared
// in projectRoot/relPath. Returns an empty (non-nil) set on parse error
// or a missing file — callers treat unparseable files as contributing
// no declarations rather than as a hard error.
//
// Memoized per path: callers that scan the same package directory
// multiple times in one pipeline run (e.g. dangling-ref per-sibling
// loops) don't pay re-parse cost.
func (i *Inspector) DeclaredTypesIn(relPath string) map[string]bool {
	if cached, ok := i.declaredTypesCache[relPath]; ok {
		return cached
	}
	content, err := os.ReadFile(filepath.Join(i.root, relPath))
	if err != nil {
		out := map[string]bool{}
		i.declaredTypesCache[relPath] = out
		return out
	}
	out := topLevelTypeNames(content)
	i.declaredTypesCache[relPath] = out
	return out
}

// topLevelTypeNames returns the set of top-level type identifiers
// declared in Go source `content`. Lower-case names are included
// because Go treats them as package-local declarations that can
// satisfy an unqualified type reference within the same package.
//
// Returns an empty (non-nil) set when the file fails to parse — a
// single broken file shouldn't blind a package-wide scan.
//
// Lives next to ExtractGoExports rather than in the cli package's
// dangling-ref check so all AST-touching ownership queries share one
// home; the dangling-ref check delegates to the inspector.
func topLevelTypeNames(content []byte) map[string]bool {
	out := map[string]bool{}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", content, parser.SkipObjectResolution)
	if err != nil {
		return out
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		if gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			out[ts.Name.Name] = true
		}
	}
	return out
}
