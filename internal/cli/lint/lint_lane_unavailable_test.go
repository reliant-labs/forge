// File: internal/cli/lint/lint_lane_unavailable_test.go
//
// Regression coverage for the defect these tests were written against: a
// `forge lint` lane that COULD NOT RUN reporting as a lane that PASSED.
//
// The typed-config guardrail shells out to golangci-lint, which takes an
// exclusive OS file lock on $TMPDIR/golangci-lint.lock — one path shared by
// every golangci-lint on the machine — and waits five seconds for it before
// exiting 3 with "parallel golangci-lint is running". An editor's LSP or a
// sibling CI job holding that lock for the second of forge's two invocations
// made the lane die; the lane caught the error, printed a ⚠️, and returned
// nil. `forge lint` exited 0 with a verdict line that named only the lanes
// that DID run, and `forge lint --json` reported "ok": true.
//
// These tests do not reproduce the lock — they reproduce what the lock does
// to the lane, which is the part `forge lint` is responsible for: a
// golangci-lint that exits non-zero without reporting. The invocation is
// always run with --issues-exit-code=0, so that is the ONLY thing a non-zero
// exit can mean.

package lint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// fakeGolangciLint puts a `golangci-lint` on PATH for the duration of the
// test whose body is the given shell script. It is how an unavailable
// golangci-lint is simulated without depending on the machine's real lock
// state (which is exactly the shared, racy thing that made this defect
// invisible in the first place).
func fakeGolangciLint(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub is POSIX-only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "golangci-lint")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// contendedLockStub is golangci-lint losing the machine-global lock, byte for
// byte: the message it prints and the code it exits with, both captured from
// a real contended run.
const contendedLockStub = `echo "Error: parallel golangci-lint is running" >&2
echo "The command is terminated due to an error: parallel golangci-lint is running" >&2
exit 3`

func testRunCtx(t *testing.T, strict bool) *lintRunCtx {
	t.Helper()
	return &lintRunCtx{ctx: context.Background(), strict: strict, paths: []string{"./..."}}
}

// ── text mode ───────────────────────────────────────────────────────────────

// TestTypedAccessGuardAdvisoryReportsUnavailable is the core assertion. The
// lane used to `return nil` here, which is the driver's word for "I ran and
// found nothing" — the single line that made the whole silent pass possible.
func TestTypedAccessGuardAdvisoryReportsUnavailable(t *testing.T) {
	fakeGolangciLint(t, contendedLockStub)

	err := runTypedAccessGuardAdvisory(context.Background(), []string{"./..."})
	if err == nil {
		t.Fatal("guardrail returned nil after golangci-lint exited 3 without reporting — " +
			"the driver reads that as a lane that ran and passed")
	}
	var unavail *laneUnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("guardrail returned a plain error %v; it must be a laneUnavailableError so the "+
			"driver can tell 'could not run' from 'ran and failed' (an advisory lane must not gate)", err)
	}
	if unavail.fixHint == "" {
		t.Error("an unavailable lane must say what to do about it; fixHint is empty")
	}
	// The remediation has to name the actual cause, or the user re-runs into
	// the same lock forever.
	if !strings.Contains(unavail.fixHint, "allow-serial-runners") {
		t.Errorf("fix hint never mentions allow-serial-runners, the one-line fix for the lock:\n%s", unavail.fixHint)
	}
}

// TestTypedAccessGuardAdvisoryPassesWhenItRuns pins the other side: a
// guardrail that actually ran must still return nil, or the new sentinel
// would turn every clean run into a warning.
func TestTypedAccessGuardAdvisoryPassesWhenItRuns(t *testing.T) {
	fakeGolangciLint(t, `echo "0 issues."; exit 0`)

	if err := runTypedAccessGuardAdvisory(context.Background(), []string{"./..."}); err != nil {
		t.Fatalf("guardrail reported a problem on a clean run: %v", err)
	}
}

// ── the verdict line ────────────────────────────────────────────────────────

// TestLintVerdictNamesLanesThatCouldNotRun guards the last line on screen —
// the one a human scrolls to and an agent greps for. A lane that could not
// run must be named there; it used to be invisible, so the run ended on a
// green line that counted only the lanes that had reported.
func TestLintVerdictNamesLanesThatCouldNotRun(t *testing.T) {
	var buf strings.Builder
	err := reportLintVerdict(&buf, []string{"golangci-lint", "buf lint"}, nil, []string{"typed-config guardrail"})
	if err != nil {
		t.Fatalf("an unavailable advisory lane must not fail the run without --strict: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "typed-config guardrail") {
		t.Errorf("verdict does not name the lane that could not run:\n%s", got)
	}
	if strings.Contains(got, "✅") {
		t.Errorf("verdict still reads as a clean pass over a lane that never ran:\n%s", got)
	}
	if !strings.Contains(got, "could NOT run") {
		t.Errorf("verdict never says the lane did not run:\n%s", got)
	}
}

// TestLintVerdictUnavailableLaneIsNotCountedAsPassed is the arithmetic half:
// the count in the verdict must not include the lane that produced no
// verdict, or the number is a lie even when the words are right.
func TestLintVerdictUnavailableLaneIsNotCountedAsPassed(t *testing.T) {
	var buf strings.Builder
	if err := reportLintVerdict(&buf, []string{"a", "b"}, nil, []string{"c"}); err != nil {
		t.Fatalf("verdict failed: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "2 gating linter(s) passed") {
		t.Errorf("verdict miscounts the lanes that actually passed:\n%s", got)
	}
	if strings.Contains(got, "3 gating") {
		t.Errorf("verdict counted the unavailable lane as passed:\n%s", got)
	}
}

// TestLintVerdictNothingRanNamesUnavailable: when NOTHING gated, the failure
// has to distinguish "the tool isn't installed" from "the tool broke", or the
// user is pointed at the wrong fix.
func TestLintVerdictNothingRanNamesUnavailable(t *testing.T) {
	var buf strings.Builder
	err := reportLintVerdict(&buf, nil, nil, []string{"typed-config guardrail"})
	if err == nil {
		t.Fatal("zero gating linters ran and the verdict succeeded")
	}
	if !strings.Contains(err.Error(), "typed-config guardrail") {
		t.Errorf("verdict error never names the lane that could not run: %v", err)
	}
}

// ── JSON mode: what a machine consumes ──────────────────────────────────────

// TestTypedAccessGuardJSONDistinguishesRanFromDidNotRun is the JSON half of
// the defect. Both states used to arrive as rule "typed-config-guardrail" at
// warning severity, so a consumer could only tell them apart by parsing
// English out of the message.
func TestTypedAccessGuardJSONDistinguishesRanFromDidNotRun(t *testing.T) {
	t.Run("could not run", func(t *testing.T) {
		fakeGolangciLint(t, contendedLockStub)

		fs, gated, err := collectTypedAccessGuardJSON(testRunCtx(t, false))
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if gated {
			t.Error("an advisory lane that could not run must not gate without --strict")
		}
		verdict := findRule(t, fs, ruleTypedAccessGuardUnavailable)
		if verdict.Severity != lintSevWarning {
			t.Errorf("unavailable verdict severity = %q, want warning", verdict.Severity)
		}
		if verdict.FixHint == "" {
			t.Error("unavailable verdict carries no fix_hint")
		}
		// The cause golangci-lint printed is the evidence. It used to be
		// captured and then dropped, leaving an exit status and no reason.
		if !strings.Contains(allMessages(fs), "parallel golangci-lint is running") {
			t.Errorf("golangci-lint's own explanation was dropped from the report:\n%s", allMessages(fs))
		}
	})

	t.Run("ran and reported", func(t *testing.T) {
		fakeGolangciLint(t, `echo "internal/x/y.go:12:3: use of os.Getenv forbidden (forbidigo)"; exit 0`)

		fs, gated, err := collectTypedAccessGuardJSON(testRunCtx(t, false))
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if gated {
			t.Error("guardrail findings are advisory and must not gate")
		}
		for _, f := range fs {
			if f.Rule == ruleTypedAccessGuardUnavailable {
				t.Fatalf("a lane that RAN was reported as unavailable: %+v", f)
			}
		}
		if len(fs) == 0 {
			t.Fatal("a guardrail run that reported a finding produced no findings")
		}
	})
}

// TestTypedAccessGuardJSONStrictFlipsOK: --strict is the documented
// escalation for "could not run", and the only thing a machine reading `ok`
// responds to. Without it, `ok: true` over a check that never executed is
// only defensible because the report NAMES the absence; with it, the report
// must actually be not-ok.
func TestTypedAccessGuardJSONStrictFlipsOK(t *testing.T) {
	fakeGolangciLint(t, contendedLockStub)

	fs, gated, err := collectTypedAccessGuardJSON(testRunCtx(t, true))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !gated {
		t.Fatal("--strict did not gate on a lane that could not run")
	}
	if got := findRule(t, fs, ruleTypedAccessGuardUnavailable).Severity; got != lintSevError {
		t.Errorf("--strict severity = %q, want error", got)
	}
	if report := buildLintJSONReport(fs, gated); report.OK {
		t.Error(`report says "ok": true under --strict over a check that never executed`)
	}
}

// ── the frontend eslint lane, same shape ────────────────────────────────────

// TestFrontendLintDirUnavailableWhenDepsMissing: this lane GATES, and a nil
// return made it count as a gating linter that passed over a frontend whose
// eslint was never invoked.
func TestFrontendLintDirUnavailableWhenDepsMissing(t *testing.T) {
	feDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(feDir, "package.json"), []byte(`{"scripts":{"lint":"eslint ."}}`), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	err := lintFrontendDir(context.Background(), "dashboard", feDir, "", false, false)
	var unavail *laneUnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("eslint lane returned %v for a frontend with no node_modules; want laneUnavailableError "+
			"(this lane gates, so nil counts it as a gating linter that passed)", err)
	}
	// scaffold_lint_clean_e2e_test.go asserts on this substring to prove the
	// frontend lane was not vacuously green; keep it findable.
	if !strings.Contains(unavail.reason, "node_modules not found") {
		t.Errorf("reason no longer contains the phrase the e2e vacuity guard greps for:\n%s", unavail.reason)
	}
}

// TestCollectFrontendLintJSONUnavailableIsNotASkip: "skipped" is the report's
// word for a lane that did not APPLY. A frontend forge was told to lint and
// could not is a different thing, and a consumer filtering out info/"skipped"
// noise would have dropped it entirely.
func TestCollectFrontendLintJSONUnavailableIsNotASkip(t *testing.T) {
	feDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(feDir, "package.json"), []byte(`{"scripts":{"lint":"eslint ."}}`), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	cfg := &config.ProjectConfig{Frontends: []config.FrontendConfig{{Name: "dashboard", Path: feDir}}}

	rc := testRunCtx(t, false)
	rc.cfg = cfg

	got, gatedNonStrict := collectFrontendLintJSON(rc)
	if gatedNonStrict {
		t.Error("a frontend that could not be linted must not gate without --strict")
	}
	f := findRule(t, got, ruleFrontendLintUnavailable)
	if f.Severity != lintSevWarning {
		t.Errorf("severity = %q, want warning", f.Severity)
	}
	for _, x := range got {
		if x.Rule == "skipped" && strings.Contains(x.Message, "node_modules") {
			t.Errorf(`missing deps still reported as rule "skipped": %+v`, x)
		}
	}

	rc.strict = true
	strictFindings, strictGated := collectFrontendLintJSON(rc)
	if !strictGated {
		t.Error("--strict did not gate on a frontend that could not be linted")
	}
	if got := findRule(t, strictFindings, ruleFrontendLintUnavailable).Severity; got != lintSevError {
		t.Errorf("--strict severity = %q, want error", got)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func findRule(t *testing.T, fs []lintJSONFinding, rule string) lintJSONFinding {
	t.Helper()
	for _, f := range fs {
		if f.Rule == rule {
			return f
		}
	}
	t.Fatalf("no finding with rule %q in:\n%s", rule, allMessages(fs))
	return lintJSONFinding{}
}

func allMessages(fs []lintJSONFinding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString("  [" + f.Severity + "] " + f.Rule + ": " + f.Message + "\n")
	}
	if b.Len() == 0 {
		return "  (no findings)\n"
	}
	return b.String()
}
