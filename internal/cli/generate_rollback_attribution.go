package cli

// generate_rollback_attribution.go — which post-rollback errors the revert
// caused, and which were there all along.
//
// The failed-generate report compiles the tree the rollback restored and
// prints what fails there. An error citing a file the run never wrote used
// to be labelled, unconditionally, an ARTIFACT OF THE ROLLBACK: "unchanged
// on disk and correct", reported only because a file it depends on was
// reverted. That holds for the case the label was written for (scoping.go
// calling a method only this run's handlers_crud_ops_gen.go defines), but
// not for a file that was already broken. `forge generate --scope
// internal/assigner` with an error planted in internal/assigner and an
// unrelated, pre-existing one in pkg/rangerclient/zz_broken.go named the
// rangerclient file a rollback artifact, correct on disk. It was broken
// before the run, and fixing the root cause would never clear it.
//
// The restored tree IS the pre-run tree (the revert is byte-faithful), so
// every error in it existed before the run. The question that separates the
// two is the other side: does the error go away once this run's output is
// in place? cmd/go answers it without touching the tree: the post-run bytes
// of every file the revert put back, captured just before it did, are laid
// over the tree with `go list -overlay` and the same ./... build scope is
// compiled once more. (forge#2928 asks the inverse question in the validate
// step, overlaying the pre-run bytes on the post-run tree.)
//
//   - An error that vanishes there is an artifact of the rollback: the file
//     is correct against this run's output.
//   - An error that is still there is pre-existing, unrelated to this run:
//     the report says so, and that fixing the root cause will not clear it.
//   - Anything that cannot be decided — the package is not compiled there
//     because a package it imports fails with the run's output, the
//     package itself fails differently there, or the check could not run — is labelled as not
//     attributed, never given either claim.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/goexec"
)

// errorOrigin is where a post-rollback compile error comes from.
type errorOrigin int

const (
	// originUnknown: not decided; the line's reason says why.
	originUnknown errorOrigin = iota
	// originRollback: the error vanishes with this run's output in place.
	originRollback
	// originPreExisting: the error is there with this run's output too.
	originPreExisting
)

// lineOrigin is one error line's attribution.
type lineOrigin struct {
	origin errorOrigin
	// why is set for originUnknown.
	why string
}

// postRollbackAttribution attributes the post-rollback compile errors. A nil
// attribution means no check ran; every untouched-file error is then
// reported as not attributed.
type postRollbackAttribution struct {
	// lines maps a trimmed compiler error line to its origin.
	lines map[string]lineOrigin
	// reason says why the check could not run at all ("" when it ran).
	reason string
}

// of returns line's attribution.
func (a *postRollbackAttribution) of(line string) lineOrigin {
	if a == nil {
		return lineOrigin{origin: originUnknown, why: "the check did not run"}
	}
	if a.reason != "" {
		return lineOrigin{origin: originUnknown, why: a.reason}
	}
	if o, ok := a.lines[strings.TrimSpace(line)]; ok {
		return o
	}
	return lineOrigin{origin: originUnknown, why: "the check did not reach it"}
}

// postRunFile is a file's state as the failed run left it, read just before
// the revert.
type postRunFile struct {
	exists  bool
	content []byte
}

// postRunGoInputs is the failed run's output of every Go build input it
// changed (project-relative slash paths), or nil with the paths it could not
// read. Read it BEFORE the revert, which puts these paths back.
type postRunGoInputs struct {
	files      map[string]postRunFile
	unreadable []string
}

// capturePostRunGoInputs reads, before the revert, the run's output of
// every changed path the compiler reads.
func capturePostRunGoInputs(root string, changed []string) postRunGoInputs {
	out := postRunGoInputs{files: map[string]postRunFile{}}
	for _, rel := range changed {
		if !isGoBuildInput(rel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		switch {
		case err == nil:
			out.files[rel] = postRunFile{exists: true, content: data}
		case errors.Is(err, os.ErrNotExist):
			out.files[rel] = postRunFile{}
		default:
			out.unreadable = append(out.unreadable, rel)
		}
	}
	return out
}

// attributePostRollbackErrors compiles the packages that fail in the
// restored tree once more, with each Go build input the revert put back
// (restored) overlaid by the bytes the run left there, and attributes every
// error line in output. Nothing on disk changes.
func attributePostRollbackErrors(projectDir string, postRun postRunGoInputs, restored []string, output string) *postRollbackAttribution {
	attr := &postRollbackAttribution{lines: map[string]lineOrigin{}}
	root, err := filepath.Abs(projectDir)
	if err != nil {
		attr.reason = "the check could not run: " + err.Error()
		return attr
	}

	replace := map[string]postRunFile{}
	for _, rel := range restored {
		rel = filepath.ToSlash(rel)
		// Restore can list journaled files rewritten with identical bytes.
		// They are not in the changed set and need no overlay entry.
		if f, changed := postRun.files[rel]; changed {
			replace[rel] = f
		}
	}
	if len(postRun.unreadable) > 0 {
		attr.reason = fmt.Sprintf("this run's output of %s could not be read before the revert", postRun.unreadable[0])
		return attr
	}

	// Each line's package, by its file's directory. A line citing a file
	// outside the project has no package here to compile.
	lines := map[string]string{}
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		file := errorLineFile(line)
		if file == "" {
			continue
		}
		dir, inside := projectRelDirOf(root, filepath.Dir(file))
		if !inside {
			attr.lines[line] = lineOrigin{why: "it cites a file outside the project"}
			continue
		}
		lines[line] = dir
	}
	if len(lines) == 0 {
		return attr
	}
	if len(replace) == 0 {
		// The revert put back no Go build input, so the tree with this
		// run's output in place compiles exactly as the restored one does.
		for line := range lines {
			attr.lines[line] = lineOrigin{origin: originPreExisting}
		}
		return attr
	}

	pkgs, err := compileWithPostRunOverlay(root, replace)
	if err != nil {
		attr.reason = "the check could not run: " + err.Error()
		return attr
	}
	byDir := map[string]*postRunPackage{}
	for _, p := range pkgs {
		if p.testBuild() {
			continue
		}
		if dir, inside := projectRelDirOf(root, p.Dir); inside {
			byDir[dir] = p
		}
	}
	for line, dir := range lines {
		attr.lines[line] = originUnder(byDir[dir], line)
	}
	return attr
}

// originUnder attributes one post-rollback error line from its package's
// compile with this run's output in place (p, nil when that compile did
// not list it).
func originUnder(p *postRunPackage, line string) lineOrigin {
	if p == nil {
		return lineOrigin{origin: originUnknown, why: "its package is not there with this run's output in place"}
	}
	if p.Error == nil {
		if len(p.DepsErrors) > 0 {
			return lineOrigin{origin: originUnknown, why: "a package it imports does not compile with this run's output either, so it could not be recompiled"}
		}
		if p.Name == "main" && strings.HasSuffix(line, ": function main is undeclared in the main package") {
			// missingMainFunc checks source files directly, not overlay bytes.
			return lineOrigin{origin: originUnknown, why: "the replay compiled this main package but did not check for func main"}
		}
		if p.Export == "" || p.Incomplete {
			return lineOrigin{origin: originUnknown, why: "the replay did not produce a compiled package"}
		}
		return lineOrigin{origin: originRollback}
	}
	if len(p.DepsErrors) > 0 || !strings.HasPrefix(p.Error.Err, "# ") {
		return lineOrigin{origin: originUnknown, why: "the replay did not reach this package's compiler"}
	}
	for _, l := range strings.Split(p.Error.text(), "\n") {
		if strings.TrimSpace(l) == line {
			return lineOrigin{origin: originPreExisting}
		}
	}
	// A package still failing for another reason can mask diagnostics (e.g.
	// cmd/compile's error cap or a syntax error stopping type checking).
	return lineOrigin{origin: originUnknown, why: "its package did not compile with this run's output, which may hide other errors"}
}

// projectRelDirOf returns the project-relative directory (slash form) of a
// directory a compiler or go list reported, and whether it lies
// inside root. Relative paths are relative to root, go list's working
// directory.
func projectRelDirOf(root, p string) (string, bool) {
	if p == "" {
		return "", false
	}
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// postRunPackage includes evidence that go list actually compiled a package.
// A listed package without an Error is not enough: dependency failures can
// prevent compilation, and excluded packages may have no buildable files.
type postRunPackage struct {
	listedPackage
	Export     string `json:"Export"`
	Incomplete bool   `json:"Incomplete"`
}

// compileWithPostRunOverlay replays the restored-tree check's ./... scope
// with each replaced path overlaid by the run's output (removed when the
// run left none). -export compiles packages; Export proves success, while
// Error carries diagnostics for packages whose own compilation failed.
func compileWithPostRunOverlay(root string, replace map[string]postRunFile) ([]*postRunPackage, error) {
	tmp, err := os.MkdirTemp("", "forge-rollback-postrun-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	overlay := make(map[string]string, len(replace))
	i := 0
	for rel, f := range replace {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if !f.exists {
			overlay[full] = "" // the run removed it
			continue
		}
		saved := filepath.Join(tmp, strconv.Itoa(i))
		i++
		if err := os.WriteFile(saved, f.content, 0o600); err != nil {
			return nil, err
		}
		overlay[full] = saved
	}
	doc, err := json.Marshal(map[string]map[string]string{"Replace": overlay})
	if err != nil {
		return nil, err
	}
	overlayFile := filepath.Join(tmp, "overlay.json")
	if err := os.WriteFile(overlayFile, doc, 0o600); err != nil {
		return nil, err
	}

	args := []string{"list", "-e", "-export", "-deps", "-json=" + compileFields + ",Export,Incomplete", "-overlay=" + overlayFile, "./..."}
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Env = goexec.CheckEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, &listFailure{stderr: stderr.String(), err: err}
	}
	var pkgs []*postRunPackage
	dec := json.NewDecoder(&stdout)
	for {
		p := new(postRunPackage)
		if err := dec.Decode(p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}
