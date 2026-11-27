package cli

// generate_validate_scope.go — the validate step fails a run for what the
// run broke, not for what was already broken.
//
// The compile check (checkGoBuild) compiles the whole module, and used to
// roll back every file the run wrote on ANY compile error. In a checkout
// several agents share, one agent's half-edited package then failed every
// other agent's generate: an agent changing its own package's deps.go to
// refresh mock_gen.go was rolled back over another agent's syntax error in
// a package the run never touched. --keep-on-build-failure kept the files,
// but still exited non-zero and printed the other agents' errors as if they
// were the run's. It was the most-hit item of the Ranger build.
//
// THE QUESTION is causal: would these errors be there without this run's
// writes? The journal already holds the answer's inputs — the pre-run bytes
// of every file the run changed (checksums.JournaledPreRun) — and cmd/go can
// compile against them without touching the tree: `-overlay` replaces file
// contents for one invocation. So:
//
//   - An error in a package the run wrote into, or in a forge-owned file
//     anywhere, is the run's, as before: rolled back (or kept, under
//     --keep-on-build-failure, and the run exits non-zero).
//   - Every other failing package — the candidates, the packages this run
//     did not write — is compiled once more with each file the run changed
//     overlaid by its pre-run bytes (or removed, when the run created it).
//     A candidate whose errors all reproduce there failed before the run
//     and fails the same way after it: not caused by generate. Anything
//     else — a new error, or one that appears only against the new bytes,
//     the case of a hand-written consumer that no longer matches a
//     regenerated interface — is the run's.
//
// When every error is pre-existing, the run keeps its files and exits 0,
// and the closing line says the project does NOT compile (validateCaveats),
// naming the errors and the packages. Such a run is never memoized and
// never recorded as a verified fixed point: the tree it ends on does not
// compile, so the next run checks it again.
//
// What this cannot check: a package this run wrote into that imports a
// broken one is not compiled at all (cmd/go needs its dependencies' export
// data), so its generated code is unverified. It did not compile before the
// run either; the report names it, and the next run — after the owner fixes
// the broken package — checks it.
//
// Fail closed everywhere: a package outside the project, an error with no
// package to attribute it to, a file the journal could not capture, a
// Go build input that changed outside the journal (its pre-run
// bytes are unknown), or the second compile failing to run — each leaves
// the whole failure the run's, exactly the behavior before this file.
// --strict never attributes: a run under it fails on any compile error.

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"github.com/reliant-labs/forge/internal/checksums"
)

// generateWriteScope is what the validate step knows about the run that
// wrote the tree it compiles.
type generateWriteScope struct {
	// written are the project-relative paths (slash form) this run's
	// writers changed: the journal's changed set.
	written []string
	// unrestorable are Go build inputs that changed during the
	// run without passing through the journal, so their pre-run bytes are
	// unknown.
	unrestorable []string
	// preRun returns a written path's pre-run state (checksums.JournaledPreRun).
	preRun func(rel string) (checksums.PreRunState, bool)
}

// writeScopeOf returns the scope the pipeline's compile check attributes
// failures with, or nil — every compile error the run's — under --strict or
// when the pre-run tree could not be listed.
func writeScopeOf(ctx *pipelineContext) *generateWriteScope {
	if ctx.Strict || ctx.preRun == nil {
		return nil
	}
	written := checksums.ChangedJournalPaths(ctx.AbsPath)
	changed, ok := ctx.preRun.changedSince(ctx.AbsPath, nil)
	if !ok {
		return nil
	}
	journaled := make(map[string]bool, len(written))
	for _, rel := range written {
		journaled[rel] = true
	}
	var unrestorable []string
	for _, rel := range changed {
		if journaled[rel] || strings.HasPrefix(rel, ".forge/") || !isGoBuildInput(rel) {
			continue
		}
		// Ownership cannot establish the pre-run bytes. Even a hand-written
		// input saved by another agent mid-run makes the comparison unsound:
		// the overlay would read its current state, not its pre-run state.
		unrestorable = append(unrestorable, rel)
	}
	return &generateWriteScope{written: written, unrestorable: unrestorable, preRun: checksums.JournaledPreRun}
}

// foreignCompile is a compile failure the run did not cause.
type foreignCompile struct {
	// output is each failing package's own errors, as go build prints them.
	output string
	// errors counts the compiler errors in output (one per load error).
	errors int
	// packages are the failing packages' import paths.
	packages []string
	// unverified are the packages this run wrote into that could not be
	// compiled because a package they import does not.
	unverified []string
}

// attribute decides whether the compile failure in pkgs is entirely
// pre-existing. It returns the failure as foreign, or ok=false when any of
// it is (or may be) the run's.
func (s *generateWriteScope) attribute(projectDir string, pkgs []*listedPackage) (_ *foreignCompile, ok bool) {
	if s == nil || len(s.unrestorable) > 0 {
		return nil, false
	}
	root, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, false
	}
	wroteInto := map[string]bool{}
	for _, rel := range s.written {
		wroteInto[path.Dir(rel)] = true
	}
	var candidates []*listedPackage
	own := map[string]string{}
	for _, p := range pkgs {
		if p.testBuild() {
			continue // test variants: validateTestFiles judges their files
		}
		text := ownCompileErrors(projectDir, p)
		if text == "" {
			continue
		}
		dir, inside := projectRelDir(root, p.Dir)
		if !inside || wroteInto[dir] {
			return nil, false
		}
		mapped := 0
		// Do not use compilerErrorLines here: it drops diagnostics without
		// a file location, which cannot establish pre-existing breakage.
		for _, line := range strings.Split(text, "\n") {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "# ") ||
				strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") ||
				strings.HasSuffix(line, ": too many errors") {
				continue
			}
			file := filepath.FromSlash(errorLineFile(line))
			if file == "" {
				return nil, false
			}
			if !filepath.IsAbs(file) {
				file = filepath.Join(root, file)
			}
			name, err := filepath.Rel(p.Dir, file)
			if err != nil || filepath.Dir(name) != "." || filepath.Ext(name) != ".go" ||
				(!slices.Contains(p.GoFiles, name) && !slices.Contains(p.CgoFiles, name)) {
				return nil, false
			}
			// goSourceOriginOf treats unreadable files as hand-written for
			// remediation hints; attribution must instead fail closed.
			if _, err := os.ReadFile(file); err != nil {
				return nil, false
			}
			if goSourceOriginOf(file) != originHandWritten {
				return nil, false
			}
			mapped++
		}
		if mapped == 0 {
			return nil, false
		}
		candidates = append(candidates, p)
		own[p.ImportPath] = text
	}
	if len(candidates) == 0 {
		return nil, false
	}
	if len(s.written) > 0 {
		before, err := s.compileBefore(projectDir, root, candidates)
		if err != nil {
			return nil, false
		}
		for _, p := range candidates {
			if !errorsReproduce(own[p.ImportPath], before[p.ImportPath]) {
				return nil, false
			}
		}
	}

	fc := &foreignCompile{}
	var texts []string
	for _, p := range candidates {
		texts = append(texts, own[p.ImportPath])
		fc.packages = append(fc.packages, p.ImportPath)
		fc.errors += max(1, len(compilerErrorLines(own[p.ImportPath])))
	}
	fc.output = strings.Join(texts, "\n") + "\n"
	fc.unverified = uncompiledReaders(root, pkgs, wroteInto)
	return fc, true
}

// uncompiledReaders returns the packages whose compile the run's writes
// reach — a package the run wrote into, or one importing such a package,
// directly or not — that were not compiled because a package they import
// failed. What the run changed in or under them is unchecked.
func uncompiledReaders(root string, pkgs []*listedPackage, wroteInto map[string]bool) []string {
	written := map[string]bool{}
	for _, p := range pkgs {
		if dir, inside := projectRelDir(root, p.Dir); inside && wroteInto[dir] && !p.testBuild() {
			written[p.ImportPath] = true
		}
	}
	var out []string
	for _, p := range pkgs {
		if p.testBuild() || p.DepOnly || p.Error != nil || len(p.DepsErrors) == 0 {
			continue
		}
		if written[p.ImportPath] || slices.ContainsFunc(p.Deps, func(dep string) bool { return written[dep] }) {
			out = append(out, p.ImportPath)
		}
	}
	return out
}

// ownCompileErrors is the package's own failure — not a dependency's — as
// go build prints it, or "" when it compiled (or was not compiled because a
// dependency failed).
func ownCompileErrors(projectDir string, p *listedPackage) string {
	if p.Error != nil {
		return p.Error.text()
	}
	if !p.DepOnly && len(p.DepsErrors) == 0 && p.Name == "main" {
		return missingMainFunc(projectDir, p)
	}
	return ""
}

// projectRelDir returns dir relative to root, slash form, and whether it
// lies inside root.
func projectRelDir(root, dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// compileBefore compiles the candidates against the tree as it was before
// the run — each file the run changed overlaid by its pre-run bytes, or
// removed when the run created it — and returns each one's own errors by
// import path. Nothing on disk changes.
func (s *generateWriteScope) compileBefore(projectDir, root string, candidates []*listedPackage) (map[string]string, error) {
	tmp, err := os.MkdirTemp("", "forge-validate-prerun-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	replace := make(map[string]string, len(s.written))
	for i, rel := range s.written {
		full := filepath.Join(root, filepath.FromSlash(rel))
		state, journaled := s.preRun(rel)
		switch {
		case !journaled || !state.Existed:
			replace[full] = "" // created by this run: absent before it
		case state.Unverifiable:
			return nil, fmt.Errorf("the pre-run bytes of %s were not captured", rel)
		default:
			saved := filepath.Join(tmp, strconv.Itoa(i))
			if err := atomicfile.WriteFile(saved, state.Content, 0o600); err != nil {
				return nil, err
			}
			replace[full] = saved
		}
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": replace})
	if err != nil {
		return nil, err
	}
	overlayFile := filepath.Join(tmp, "overlay.json")
	if err := atomicfile.WriteFile(overlayFile, overlay, 0o600); err != nil {
		return nil, err
	}
	patterns := make([]string, len(candidates))
	for i, p := range candidates {
		patterns[i] = p.ImportPath
	}
	pkgs, err := listCompiled(projectDir, false, []string{"-overlay=" + overlayFile}, nil, patterns...)
	if err != nil {
		return nil, err
	}
	before := map[string]string{}
	for _, p := range pkgs {
		if !p.testBuild() {
			before[p.ImportPath] = ownCompileErrors(projectDir, p)
		}
	}
	return before, nil
}

// errorsReproduce reports whether every error line in after also appears in
// before: the package failed the same way before the run. The `# package`
// headers and the compiler's truncation notice are not errors.
func errorsReproduce(after, before string) bool {
	if strings.TrimSpace(before) == "" {
		return false
	}
	lines := func(text string) []string {
		var out []string
		for _, l := range strings.Split(text, "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "# ") || strings.HasSuffix(l, ": too many errors") {
				continue
			}
			out = append(out, l)
		}
		return out
	}
	was := lines(before)
	for _, l := range lines(after) {
		if !slices.Contains(was, l) {
			return false
		}
	}
	return true
}

// report prints the foreign failure where the compile check prints a
// failure, and says why the run goes on.
func (fc *foreignCompile) report(w *os.File) {
	fmt.Fprint(w, fc.output)
	fmt.Fprintf(w, "\n⚠️  %d compile error(s) in %d package(s) this run did not write: %s — not caused by generate "+
		"(each fails the same way against the tree as it was before this run), so the run's files are kept.\n",
		fc.errors, len(fc.packages), strings.Join(fc.packages, ", "))
	if len(fc.unverified) > 0 {
		fmt.Fprintf(w, "   UNCHECKED: %d package(s) this run's changes reach (it wrote into them, or they import one it did) "+
			"import those packages, so they could not be compiled: %s. Whether this run's output compiles there is unknown "+
			"until the errors above are fixed; the next `%s generate` checks it.\n",
			len(fc.unverified), listSome(fc.unverified), Name())
	}
}
