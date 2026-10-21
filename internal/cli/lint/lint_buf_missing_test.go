package lint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBufLaneFailsWithoutBuf pins the buf half of "a gating lane that did not
// run cannot pass": a project with a buf.yaml on a machine with no buf used to
// SKIP the buf lint lane, and `forge lint` passed without linting a proto. The
// lane must report that it could NOT run, naming the fix, and that fails the
// run (lint_skip.go) — in text and JSON mode.
func TestBufLaneFailsWithoutBuf(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "buf.yaml"), []byte("version: v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("PATH", t.TempDir())

	step := findStep(t, "buf lint")
	rc := &lintRunCtx{ctx: context.Background(), cwd: dir}
	if run, skip := step.shouldRun(rc); !run {
		t.Fatalf("buf lint skipped with no buf on PATH (%q): a gating lane must never skip for a missing tool", skip)
	}
	err := step.runText(rc)
	var unavail *laneUnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("buf lint returned %v with no buf on PATH; want a lane that could NOT run", err)
	}
	if !strings.Contains(unavail.reason, "buf lint lane cannot run") || !strings.Contains(unavail.fixHint, "forge tools install buf") {
		t.Errorf("want the reason and the fix, got reason=%q fix=%q", unavail.reason, unavail.fixHint)
	}
	fs, gated, cerr := collectGraded(t, step, rc)
	if cerr != nil || !gated || len(fs) != 1 || fs[0].Severity != lintSevError {
		t.Errorf("json: want one gating error finding, got gated=%v err=%v %+v", gated, cerr, fs)
	}
}

// TestBufLaneWithoutBufYAMLNeedsNoBuf: a project with no buf.yaml has no
// proto to lint, so a missing buf is not a failure there.
func TestBufLaneWithoutBufYAMLNeedsNoBuf(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PATH", t.TempDir())
	step := findStep(t, "buf lint")
	rc := &lintRunCtx{ctx: context.Background()}
	if err := step.runText(rc); err != nil {
		t.Errorf("no buf.yaml: want a pass, got %v", err)
	}
	if fs, gated, err := step.collect(rc); err != nil || gated || len(fs) != 0 {
		t.Errorf("no buf.yaml: want no findings, got gated=%v err=%v %+v", gated, err, fs)
	}
}
