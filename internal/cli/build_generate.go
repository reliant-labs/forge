package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/mod/modfile"
)

// ensureGeneratedCode runs the generate pipeline when the project's
// generated code is missing or stale, so a downstream `go build` doesn't
// fail with the cryptic go.work error a fresh checkout hits:
//
//	go: cannot load module gen listed in go.work file: open gen/go.mod: no such file or directory
//
// It is wired into runBuild (the shared build choke point for `forge
// build` and `forge up`'s build phase) and into runUp's host-only path,
// where the host runners (air / go-run) compile against gen/ too even
// though the docker build phase is skipped.
//
// No-op when skip is true (--no-generate), when the project declares no
// codegen surface (no go.work module dirs and no proto/), or when the
// generated tree is already up to date. The staleness gate matters: a
// full `forge generate` is heavy and noisy, so we only pay it when we'd
// otherwise hand the user a confusing build failure.
func ensureGeneratedCode(projectDir string, skip bool) error {
	if skip {
		return nil
	}
	reason, needs := generatedCodeNeedsRefresh(projectDir)
	if !needs {
		return nil
	}
	fmt.Printf("[build] generated code %s — running `forge generate` first (pass --no-generate to skip)\n", reason)
	generateMu.Lock()
	err := runGeneratePipelineFlags(projectDir, pipelineFlags{})
	generateMu.Unlock()
	if err != nil {
		return fmt.Errorf("auto-generate before build: %w", err)
	}
	return nil
}

// generatedCodeNeedsRefresh reports whether forge-generated code is
// missing or stale relative to its proto sources, returning a short
// human reason (for the build log) alongside the boolean.
//
// Two triggers, in priority order:
//
//  1. Missing module — a go.work-listed directory (other than the main
//     module) has no go.mod. This is the hard-fail case: `go build`
//     aborts immediately with "cannot load module … listed in go.work".
//     Hits every fresh checkout when gen/ is gitignored, and any tree
//     after `forge generate`'s clean sweep removed gen/.
//
//  2. Stale output — proto sources are newer than the generated tree.
//     Only consulted when both proto/ and gen/ exist; a clean git
//     checkout stamps both at the same time, so this stays quiet unless
//     a developer actually edited a .proto without regenerating.
func generatedCodeNeedsRefresh(projectDir string) (string, bool) {
	if missing := missingGoWorkModule(projectDir); missing != "" {
		return fmt.Sprintf("module %q is missing (listed in go.work, no go.mod)", missing), true
	}
	protoNewest, okProto := newestModTime(filepath.Join(projectDir, "proto"))
	genNewest, okGen := newestModTime(filepath.Join(projectDir, "gen"))
	if okProto && okGen && protoNewest.After(genNewest) {
		return "is stale (proto sources newer than gen/)", true
	}
	return "", false
}

// missingGoWorkModule returns the first go.work-listed module directory
// (other than the main module ".") whose go.mod is absent, or "" when
// the project has no go.work or every listed module is present. A use'd
// directory without a go.mod is exactly what `go build` rejects with
// "cannot load module <dir> listed in go.work file".
func missingGoWorkModule(projectDir string) string {
	path := filepath.Join(projectDir, "go.work")
	data, err := os.ReadFile(path)
	if err != nil {
		return "" // no go.work → nothing to verify
	}
	wf, err := modfile.ParseWork(path, data, nil)
	if err != nil {
		return "" // malformed go.work — let `go build` surface the real error
	}
	for _, use := range wf.Use {
		dir := filepath.Clean(use.Path)
		if dir == "." || dir == "" {
			continue
		}
		modPath := filepath.Join(projectDir, dir, "go.mod")
		if _, err := os.Stat(modPath); err != nil {
			return dir
		}
	}
	return ""
}

// newestModTime walks dir and returns the most recent file modtime. The
// bool is false when dir doesn't exist or contains no files. Symlinks and
// unreadable entries are skipped rather than treated as errors — a best-
// effort staleness signal, not a build-correctness gate.
func newestModTime(dir string) (time.Time, bool) {
	var newest time.Time
	found := false
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if mt := info.ModTime(); mt.After(newest) {
			newest = mt
			found = true
		}
		return nil
	})
	return newest, found
}
