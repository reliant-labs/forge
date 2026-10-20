package lint

// Report-only lint: the fixers' dry run.
//
// `forge lint` auto-fixes by default (#843), which is right on a developer's
// machine and wrong on a CI runner: the checkout is thrown away, so a
// formatter that quietly repairs it turns "this commit is unformatted" into a
// green build and the repair is lost. In report-only mode nothing is written;
// every file a fixer WOULD rewrite is printed as the diff the rewrite would
// apply, and the run fails, so CI fails on unformatted code and tells the
// author exactly what to change.
//
// WHEN. resolveFixMode, strongest first:
//
//	--check / --no-fix   report-only, anywhere
//	--fix                fix, anywhere (including under CI)
//	CI is set            report-only (buildinfo.IsCI: CI, GITHUB_ACTIONS)
//	otherwise            fix
//
// WHAT IS REPORTED. The pre-pass fixers that rewrite source: the Go
// formatter (gofmt, plus goimports' grouping when the project's golangci-lint
// config enables it) and prettier over a frontend's owned sources. They share
// their walk with the fix pass (walkGoFormat, ownedFrontendSources), so the
// two cannot disagree about which files are pending. The fixers that live
// inside a linter (golangci-lint --fix, eslint --fix) are simply not asked to
// fix: the linter reports the same issues as findings and fails the lane, as
// it always did under --no-fix.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/config"
)

// formatLaneName is the lane report-only mode adds for pending formatting.
const formatLaneName = "format"

// ciReportOnlyNote is said once when CI, not a flag, chose report-only.
const ciReportOnlyNote = "forge lint: CI is set, so this run is report-only: nothing is written, and any file a fixer would " +
	"rewrite fails the run with the diff it would apply. Pass --fix to apply fixes here."

// resolveFixMode decides whether this run rewrites files. note is non-empty
// when CI alone chose report-only, so the run can say why it did not fix.
func resolveFixMode(f lintFlags) (fix bool, note string) {
	switch {
	case f.noFix || f.check:
		return false, ""
	case f.fix:
		return true, ""
	case buildinfo.IsCI():
		return false, ciReportOnlyNote
	}
	return true, ""
}

// fixModeNote is resolveFixMode's note.
func fixModeNote(f lintFlags) string {
	_, note := resolveFixMode(f)
	return note
}

// autoFix reports whether this run rewrites files.
func (f lintFlags) autoFix() bool {
	fix, _ := resolveFixMode(f)
	return fix
}

// formatPending is one file a fixer would rewrite, and the rewrite.
type formatPending struct {
	File string // project-relative, slash-separated
	Tool string // what would rewrite it
	Diff string // unified diff of the rewrite
}

// unifiedDiff renders the change from a to b for the file rel.
func unifiedDiff(rel string, a, b []byte) string {
	d, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(a)),
		B:        difflib.SplitLines(string(b)),
		FromFile: "a/" + rel,
		ToFile:   "b/" + rel,
		Context:  3,
	})
	if err != nil {
		return fmt.Sprintf("(diff unavailable: %v)\n", err)
	}
	return d
}

// collectFormatPending lists every file the fix pre-pass would rewrite under
// the same bounds, without writing any. keep bounds the Go files (nil is all
// of them); withFrontends adds prettier's owned frontend sources. Best
// effort, like the fixer: a tool that cannot run contributes an error and
// whatever was found before it.
func collectFormatPending(ctx context.Context, root string, keep func(rel string) bool, withFrontends bool, cfg *config.ProjectConfig) ([]formatPending, error) {
	var out []formatPending
	err := walkGoFormat(root, keep, func(rel, _ string, src, formatted []byte) error {
		out = append(out, formatPending{File: rel, Tool: "gofmt/goimports", Diff: unifiedDiff(rel, src, formatted)})
		return nil
	})
	if withFrontends {
		fe, ferr := collectFrontendFormatPending(ctx, root, cfg)
		out = append(out, fe...)
		if err == nil {
			err = ferr
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, err
}

// collectFrontendFormatPending is formatFrontendTrees without the write:
// prettier names the files it would change, and formats each one on stdin to
// produce the diff.
func collectFrontendFormatPending(ctx context.Context, root string, cfg *config.ProjectConfig) ([]formatPending, error) {
	var out []formatPending
	for _, fe := range frontendTypecheckTargets(cfg) {
		feDir := filepath.Join(root, fe.dir)
		bin := resolveLocalBin("prettier", feDir, root)
		if bin == "" {
			continue
		}
		files, err := ownedFrontendSources(root, feDir)
		if err != nil {
			return out, fmt.Errorf("%s: %w", fe.name, err)
		}
		for start := 0; start < len(files); start += prettierBatch {
			end := min(start+prettierBatch, len(files))
			differ, err := runPrettierListDifferent(ctx, bin, feDir, files[start:end])
			if err != nil {
				return out, fmt.Errorf("%s: prettier: %w", fe.name, err)
			}
			for _, rel := range differ {
				project := filepath.ToSlash(filepath.Join(fe.dir, rel))
				diff := "(prettier would reformat this file)\n"
				if src, rerr := os.ReadFile(filepath.Join(feDir, rel)); rerr == nil {
					if formatted, perr := runPrettierStdin(ctx, bin, feDir, rel, src); perr == nil {
						diff = unifiedDiff(project, src, formatted)
					}
				}
				out = append(out, formatPending{File: project, Tool: "prettier", Diff: diff})
			}
		}
	}
	return out, nil
}

// runPrettierListDifferent is `prettier --list-different`: it names the files
// that are not formatted and writes nothing. Exit 1 means "some differ", and
// 2 that some file could not be parsed (the rest are still listed); neither
// is a failure to run.
func runPrettierListDifferent(ctx context.Context, bin, feDir string, files []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, prettierTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"--list-different"}, files...)...)
	cmd.Dir = feDir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ctx.Err() != nil || !errors.As(err, &exitErr) || (exitErr.ExitCode() != 1 && exitErr.ExitCode() != 2) {
			return nil, err
		}
	}
	var out []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// runPrettierStdin formats src as the file rel (which picks the parser and
// the project's prettier config) and returns the result.
func runPrettierStdin(ctx context.Context, bin, feDir, rel string, src []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, prettierTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--stdin-filepath", rel)
	cmd.Dir = feDir
	cmd.Stdin = bytes.NewReader(src)
	return cmd.Output()
}

// formatFindings turns pending rewrites into findings for the structured
// (--quiet / --scope) report: one anchored finding per file, then the diffs.
func formatFindings(pending []formatPending) []lintJSONFinding {
	var fs []lintJSONFinding
	for _, p := range pending {
		fs = append(fs, lintJSONFinding{
			File:     p.File,
			Severity: lintSevError,
			Rule:     "format",
			Message:  p.Tool + " would reformat this file",
			FixHint:  "run `forge lint --fix` to apply it",
		})
	}
	for _, p := range pending {
		fs = append(fs, lintJSONFinding{Severity: lintSevError, Rule: "external", Message: strings.TrimRight(p.Diff, "\n")})
	}
	return fs
}

// formatOutcome is the report-only lane in the structured drivers' terms.
func formatOutcome(ctx context.Context, root string, cfg *config.ProjectConfig, scope *lintScope, paths []string, skipFrontends bool) lintStepOutcome {
	keep := formatKeep(scope, paths)
	pending, err := collectFormatPending(ctx, root, keep, formatWithFrontends(scope, paths, skipFrontends), cfg)
	fs := formatFindings(pending)
	if err != nil {
		fs = append(fs, lintJSONFinding{Severity: lintSevWarning, Rule: "format-unavailable", Message: err.Error()})
	}
	return lintStepOutcome{name: formatLaneName, gates: true, ran: true, findings: fs, gated: len(pending) > 0}
}

// formatKeep is the file filter the Go formatter honours: the --scope when
// given, else the positional package paths.
func formatKeep(scope *lintScope, paths []string) func(rel string) bool {
	if scope != nil {
		return scope.contains
	}
	return keepForPaths(paths)
}

// formatWithFrontends reports whether the frontend formatter applies: it
// cannot be confined to Go paths or a scope, so it runs only for the whole
// project, and not under --skip-frontends.
func formatWithFrontends(scope *lintScope, paths []string, skipFrontends bool) bool {
	return scope == nil && !skipFrontends && keepForPaths(paths) == nil
}

// printFormatPending prints what report-only mode found, diffs included.
func printFormatPending(w io.Writer, pending []formatPending) {
	_, _ = fmt.Fprintf(w, "❌ %s — %d file(s) not formatted; a fix would rewrite them (nothing was written; `forge lint --fix` applies it):\n",
		formatLaneName, len(pending))
	for _, p := range pending {
		_, _ = fmt.Fprintf(w, "   %s (%s)\n", p.File, p.Tool)
	}
	_, _ = fmt.Fprintln(w)
	for _, p := range pending {
		_, _ = fmt.Fprint(w, p.Diff)
		if !strings.HasSuffix(p.Diff, "\n") {
			_, _ = fmt.Fprintln(w)
		}
	}
}

// runFormatCheck is the text driver's report-only pass: it prints the diffs
// and returns how many files are pending.
func runFormatCheck(ctx context.Context, w io.Writer, root string, paths []string, skipFrontends bool, cfg *config.ProjectConfig) int {
	pending, err := collectFormatPending(ctx, root, keepForPaths(paths), formatWithFrontends(nil, paths, skipFrontends), cfg)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "⚠️  format check: %v\n", err)
	}
	if len(pending) > 0 {
		printFormatPending(w, pending)
	}
	return len(pending)
}
