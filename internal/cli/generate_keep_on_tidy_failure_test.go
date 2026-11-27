// --keep-on-build-failure over a root `go mod tidy` that cannot resolve an
// import (forge report 1d837dcf).
//
// The defect: hand-written code imported a forge package no forge release
// had yet (pkg/siteaudit, on forge main only) — the "not expected to compile
// yet" case the flag documents. But the root tidy trips over that import
// before the compile check runs, and its failure rolled the whole run back,
// so the flag never took effect.
//
// Pinned contract:
//
//   - Default: the unresolved-import tidy failure rolls the tree back, and
//     the failure report names --keep-on-build-failure.
//   - With the flag: it is a failed step the run goes on past — the files
//     stay, the unresolved imports are repeated at the end, the run exits
//     non-zero and is never recorded as a verified fixed point.
//   - A tidy failure that is not an unresolved import (a broken go.mod or
//     go.sum) still rolls back with the flag: the module itself is broken.
package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/cli/factory"
)

const rootTidyStep = "go mod tidy (root)"

// stubGoModTidyRoot replaces the root tidy with fn.
func stubGoModTidyRoot(t *testing.T, fn func(string) error) {
	t.Helper()
	prev := goModTidyRoot
	goModTidyRoot = fn
	t.Cleanup(func() { goModTidyRoot = prev })
}

// unreleasedImportTidyError is what the root tidy reports for code that
// imports a forge package no release has yet.
func unreleasedImportTidyError() error {
	return &tidyImportError{
		cause: "a dependency cannot be resolved:\n     example.com/demo/internal/siteauditor imports github.com/reliant-labs/forge/pkg/siteaudit: " +
			"module github.com/reliant-labs/forge/pkg@latest found (v0.1.15), but does not contain package github.com/reliant-labs/forge/pkg/siteaudit",
		err: errors.New("exit status 1"),
	}
}

func TestStepGoModTidyRootKeepsOnUnresolvedImport(t *testing.T) {
	stubGoModTidyRoot(t, func(string) error { return unreleasedImportTidyError() })

	plain := &pipelineContext{ProjectDir: t.TempDir()}
	if err := stepGoModTidyRoot(plain); err == nil {
		t.Fatal("without the flag a failed tidy must fail the step, so the run rolls back")
	}
	if len(plain.SoftFailures) != 0 || plain.keptTidyFailure != nil {
		t.Errorf("without the flag nothing is kept: soft=%v kept=%v", plain.SoftFailures, plain.keptTidyFailure)
	}

	kept := &pipelineContext{ProjectDir: t.TempDir(), KeepOnBuildFailure: true}
	stderr, restore := captureStderr(t)
	err := stepGoModTidyRoot(kept)
	restore()
	if err != nil {
		t.Fatalf("with the flag the run must go on past a tidy that cannot resolve an import, got %v", err)
	}
	if !slices.Equal(kept.SoftFailures, []string{rootTidyStep}) {
		t.Errorf("the failed tidy must be a failed step of the run, got %v", kept.SoftFailures)
	}
	if kept.keptTidyFailure == nil {
		t.Error("the tidy error must be kept for the report the run ends with")
	}
	if !strings.Contains(stderr.String(), "pkg/siteaudit") {
		t.Errorf("the failure must be printed where it happened:\n%s", stderr.String())
	}
	var stepsFailed *factory.StepsFailedError
	if err := softFailureOutcome(&strings.Builder{}, kept.SoftFailures, kept.softFailureCauses); !errors.As(err, &stepsFailed) {
		t.Errorf("a kept tidy failure must still fail the run, got %v", err)
	}

	// A broken module is not a tree ahead of its code: it still rolls back.
	stubGoModTidyRoot(t, func(string) error {
		return errors.New("go.sum: checksum mismatch (fix go.mod or go.sum; go's output is above)")
	})
	broken := &pipelineContext{ProjectDir: t.TempDir(), KeepOnBuildFailure: true}
	if err := stepGoModTidyRoot(broken); err == nil {
		t.Error("a tidy failure that is not an unresolved import must fail the step even with the flag")
	}
	if len(broken.SoftFailures) != 0 || broken.keptTidyFailure != nil {
		t.Errorf("a broken module is never kept: soft=%v kept=%v", broken.SoftFailures, broken.keptTidyFailure)
	}
}

func TestKeepOnBuildFailureReportsAndHintsTidyFailures(t *testing.T) {
	var hint strings.Builder
	hintKeepOnBuildFailure(&hint, errors.Join(errors.New(`step "go mod tidy (root)"`), unreleasedImportTidyError()))
	if !strings.Contains(hint.String(), "--keep-on-build-failure") {
		t.Errorf("a rolled-back unresolved-import tidy failure must name the flag, got %q", hint.String())
	}

	var b strings.Builder
	stderr, restore := captureStderr(t)
	reportKeptBuildFailure(&b, unreleasedImportTidyError(), migrationCompileError())
	restore()
	if !strings.Contains(b.String(), "KEPT on disk") || !strings.Contains(b.String(), "2 failure(s)") {
		t.Errorf("the report must say the files were kept, counting both failures:\n%s", b.String())
	}
	for _, want := range []string{"pkg/siteaudit", "undefined: connect.NewRequest"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the report must repeat %q last:\n%s", want, stderr.String())
		}
	}

	c := runCaveats{tidyKept: true, buildKept: true, failedSteps: 2}
	if got := c.headline(); got != "the project does NOT compile" {
		t.Errorf("a kept tidy and compile failure are the one failure named, got %q", got)
	}
}

// TestGenerate_KeepOnBuildFailureKeepsOutputPastAnUnresolvedImport drives
// the real pipeline driver, journal and cache over the production root tidy
// step with the tidy failing on an import nothing resolves.
func TestGenerate_KeepOnBuildFailureKeepsOutputPastAnUnresolvedImport(t *testing.T) {
	f := newCacheFixture(t) // slow: builds a git repo; skipped under -short
	withCacheSystem(t, f)
	origCheck := restoredTreeCompileCheck
	restoredTreeCompileCheck = func(string) rollbackConsistency { return rollbackConsistency{Checked: true, Builds: true} }
	t.Cleanup(func() { restoredTreeCompileCheck = origCheck })

	const genRel = "internal/widget/widget_gen.go"
	const genBody = "package widget // generated against forge main\n"
	emitter := GenStep{
		Name: "emit output",
		Gate: always,
		Tag:  "codegen",
		Run: func(ctx *pipelineContext) error {
			checksums.RecordPreWrite(ctx.AbsPath, genRel)
			path := filepath.Join(ctx.AbsPath, filepath.FromSlash(genRel))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			return os.WriteFile(path, []byte(genBody), 0o644)
		},
	}
	withPipelineSteps(t, []GenStep{emitter, productionStep(t, rootTidyStep)})
	resolves := false
	stubGoModTidyRoot(t, func(string) error {
		if resolves {
			return nil
		}
		return unreleasedImportTidyError()
	})

	run := func(flags pipelineFlags) (string, error) {
		t.Helper()
		var runErr error
		stdout, stderr := captureStdoutAndStderr(t, func() { runErr = runGeneratePipelineFlags(f.root, flags) })
		return stdout + stderr, runErr
	}
	genPath := filepath.Join(f.root, filepath.FromSlash(genRel))

	// Default: rolled back, and the report names the flag.
	out, err := run(pipelineFlags{Cache: true})
	if err == nil {
		t.Fatalf("a failed tidy must fail the run:\n%s", out)
	}
	if _, statErr := os.Stat(genPath); !os.IsNotExist(statErr) {
		t.Errorf("without the flag the run's output must be rolled back, stat err = %v", statErr)
	}
	if !strings.Contains(out, "--keep-on-build-failure") {
		t.Errorf("a rolled-back unresolved-import tidy failure must name --keep-on-build-failure:\n%s", out)
	}

	// The flag: the output stays, the failure is reported, the run fails.
	out, err = run(pipelineFlags{Cache: true, KeepOnBuildFailure: true})
	var stepsFailed *factory.StepsFailedError
	if !errors.As(err, &stepsFailed) || !slices.Contains(stepsFailed.Steps, rootTidyStep) {
		t.Fatalf("a kept tidy failure must still exit non-zero, naming the step; got %v\n%s", err, out)
	}
	if got, readErr := os.ReadFile(genPath); readErr != nil || string(got) != genBody {
		t.Fatalf("with the flag the generated file must stay on disk, got %q (%v)\n%s", got, readErr, out)
	}
	for _, want := range []string{"KEPT on disk", "pkg/siteaudit", "the project does NOT compile"} {
		if !strings.Contains(out, want) {
			t.Errorf("the run must report %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Recorded as a verified fixed point") || loadGenerateCacheManifest(f.root) != nil {
		t.Fatalf("a run whose tidy failed must never be recorded as a verified fixed point:\n%s", out)
	}

	// Once the import resolves, a plain run passes and records.
	resolves = true
	out, err = run(pipelineFlags{Cache: true})
	if err != nil {
		t.Fatalf("a run whose tidy passes must pass: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Recorded as a verified fixed point") {
		t.Errorf("a passing run records:\n%s", out)
	}
}
