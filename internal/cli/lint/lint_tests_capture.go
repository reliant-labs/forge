// File: internal/cli/lint/lint_tests_capture.go
//
// tests-capture — a step of every `forge lint`, and part of `forge lint
// --tests`.
//
// A Taskfile test lane that runs `go test` without `forge test capture`
// records nothing: `forge test report` sees no results or coverage, flake
// detection has no history, and the release's tests gate reads an empty
// record. A project scaffolded before v1.9.0 has such lanes until the tests
// playbook moves them (internal/linter/testscapture says what counts).
//
// ADVISORY at every pin (lint_rollout_table.go): recording is report-only
// by default, so the step never gates on its own; lint.rules: {tests-capture:
// error} makes a finding fail the run, as forge, control-plane and reliant
// set it.

package lint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/linter/suppress"
	"github.com/reliant-labs/forge/internal/linter/testscapture"
)

// testsCaptureTaskfile is the Taskfile the lane reads: the project root's,
// the one every scaffolded lane, CI workflow and `forge test run` uses.
const testsCaptureTaskfile = "Taskfile.yml"

// collectTestsCaptureJSON maps the rule's findings onto the JSON contract.
// It never gates by itself: lint.rules raises it.
func collectTestsCaptureJSON(root string) ([]lintJSONFinding, bool, error) {
	src, err := os.ReadFile(filepath.Join(root, testsCaptureTaskfile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("tests-capture lint failed: %w", err)
	}
	raw := testscapture.CheckTaskfile(testsCaptureTaskfile, src)
	res := suppress.ApplyFiles(root, raw, os.ReadFile)
	return findingsToJSON(append(res.Kept, res.Violations...)), false, nil
}

// collectTestLanesJSON is `forge lint --tests`: the test-presence rules and
// tests-capture, the rules about a project's own tests.
func collectTestLanesJSON(root string) ([]lintJSONFinding, bool, error) {
	out, gated, err := collectTestsJSON(root)
	if err != nil {
		return nil, false, err
	}
	capture, _, err := collectTestsCaptureJSON(root)
	if err != nil {
		return nil, false, err
	}
	return append(out, capture...), gated, nil
}
