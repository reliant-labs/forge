package lint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/testscapture"
)

const uncapturedTaskfile = `version: "3"
tasks:
  test:
    cmds:
      - go test -count=1 ./...
`

// tests-capture warns at every pin and never gates by itself; lint.rules
// raises it, as it does flake-check.
func TestTestsCaptureIsAdvisoryUntilLintRulesRaisesIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, testsCaptureTaskfile), []byte(uncapturedTaskfile), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, gated, err := collectTestsCaptureJSON(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || fs[0].Rule != testscapture.Rule || fs[0].Severity != lintSevWarning || gated {
		t.Fatalf("collect = %+v (gated %v), want one ungated %s warning", fs, gated, testscapture.Rule)
	}
	for _, c := range []struct {
		name  string
		cfg   *config.ProjectConfig
		gates bool
	}{
		{"no config", nil, false},
		{"a pin far past its release", &config.ProjectConfig{ForgeVersion: "v1.20.0"}, false},
		{"error", &config.ProjectConfig{Lint: config.LintConfig{Rules: map[string]string{testscapture.Rule: "error"}}}, true},
		{"warn", &config.ProjectConfig{Lint: config.LintConfig{Rules: map[string]string{testscapture.Rule: "warn"}}}, false},
		{"wildcard", &config.ProjectConfig{Lint: config.LintConfig{Rules: map[string]string{"*": "error"}}}, true},
	} {
		graded, g := gradeProjectLane(c.cfg, fs, gated)
		if g != c.gates {
			t.Errorf("%s: gates = %v, want %v (%+v)", c.name, g, c.gates, graded)
		}
	}
	off := &config.ProjectConfig{Lint: config.LintConfig{Rules: map[string]string{testscapture.Rule: "off"}}}
	if graded, g := gradeProjectLane(off, fs, gated); len(graded) != 0 || g {
		t.Errorf("off: %+v (gated %v), want nothing", graded, g)
	}
	if ro, ok := lintRollouts[testscapture.Rule]; !ok || ro.GatesFrom != "" {
		t.Errorf("tests-capture's row must be advisory (no GatesFrom): %+v", ro)
	}
}

// A project with no Taskfile has nothing to report, and `forge lint --tests`
// carries the rule beside the test-presence rules.
func TestTestsCaptureLaneReadsTheRootTaskfile(t *testing.T) {
	dir := t.TempDir()
	if fs, _, err := collectTestsCaptureJSON(dir); err != nil || len(fs) != 0 {
		t.Fatalf("no Taskfile: %+v, %v", fs, err)
	}
	if err := os.WriteFile(filepath.Join(dir, testsCaptureTaskfile), []byte(uncapturedTaskfile), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, _, err := collectTestLanesJSON(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range fs {
		found = found || (f.Rule == testscapture.Rule && f.File == testsCaptureTaskfile && f.Line == 5)
	}
	if !found {
		t.Errorf("--tests findings %+v lack tests-capture at Taskfile.yml:5", fs)
	}
}
