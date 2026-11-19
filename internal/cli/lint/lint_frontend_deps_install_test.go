package lint

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/hostslot"
)

// TestMain keeps the lanes hermetic: should a test reach a real install (only
// an opted-in --install does), FORGE_SKIP_NPM_INSTALL makes it a no-op. Tests
// of the install itself clear it.
func TestMain(m *testing.M) {
	os.Setenv("FORGE_SKIP_NPM_INSTALL", "1")
	// Nor does an in-process `forge lint` take or wait for one of the host's
	// heavy slots (pkg/hostslot).
	os.Setenv(hostslot.SlotsEnv(hostslot.Heavy), "off")
	// The fix guard's type-check execs `go list`; the format and tracing
	// fixtures here are not what it is for (lint_fix_guard_test.go is).
	fixTypecheck = func(string, []string) map[string]string { return nil }
	os.Exit(m.Run())
}

// fakeNPM puts an `npm` stub on PATH that records its argv and, for install
// verbs, creates node_modules (or fails with installExit).
func fakeNPM(t *testing.T, installExit int) (calls string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	t.Setenv("FORGE_SKIP_NPM_INSTALL", "")
	bin := t.TempDir()
	calls = filepath.Join(t.TempDir(), "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> " + calls + "\n" +
		"case \"$1\" in ci|install)\n"
	if installExit == 0 {
		script += "mkdir -p node_modules\n"
	}
	script += "exit " + strconv.Itoa(installExit) + " ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func freshFrontend(t *testing.T, pkg string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"package.json": pkg, "package-lock.json": "{}"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

// fakeInstaller replaces the opted-in install for one test: it records each
// call, prints what a package manager prints, and creates node_modules (or
// fails with err).
func fakeInstaller(t *testing.T, err error) (calls *[]string) {
	t.Helper()
	calls = &[]string{}
	prev := installFrontendDeps
	installFrontendDeps = func(_ context.Context, out io.Writer, name, dir string) error {
		*calls = append(*calls, name)
		if out != nil {
			_, _ = io.WriteString(out, "Done in 415ms using pnpm v12.15.0\n")
		}
		if err != nil {
			return err
		}
		return os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755)
	}
	t.Cleanup(func() { installFrontendDeps = prev })
	return calls
}

// installed gives dir a node_modules that is current against its manifests.
func installed(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, f := range []string{"package.json", "package-lock.json"} {
		if err := os.Chtimes(filepath.Join(dir, f), old, old); err != nil {
			t.Fatal(err)
		}
	}
}

// ── the decision: lint never installs unless the run opts in ────────────────

// A frontend with no node_modules is a lane that could NOT run, naming both
// the install command and --install — and nothing was installed: a linter
// does not mutate the checkout it judges.
func TestFrontendDepsMissingDoesNotInstall(t *testing.T) {
	calls := fakeInstaller(t, nil)
	dir := freshFrontend(t, `{"scripts":{"lint":"eslint ."}}`)

	got := frontendDepsPolicy{}.check(context.Background(), "web", dir, dir, "eslint")
	if !got.missing {
		t.Fatalf("missing node_modules must be a lane that could not run, got %+v", got)
	}
	if len(*calls) != 0 {
		t.Fatalf("lint installed without --install: %v", *calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil {
		t.Fatal("lint created node_modules without --install")
	}
	for _, want := range []string{"did NOT run", "node_modules not found", "does not install"} {
		if !strings.Contains(got.reason, want) {
			t.Errorf("reason %q does not say %q", got.reason, want)
		}
	}
	for _, want := range []string{"npm ci", "forge lint --install"} {
		if !strings.Contains(got.fix, want) {
			t.Errorf("fix %q does not name %q", got.fix, want)
		}
	}
}

// Installed and current: the lane runs, with nothing to say and nothing
// installed — --install included, which only fills what is missing or stale.
func TestFrontendDepsCurrentRunsWithoutInstalling(t *testing.T) {
	calls := fakeInstaller(t, nil)
	dir := freshFrontend(t, `{"scripts":{"lint":"eslint ."}}`)
	installed(t, dir)

	for _, install := range []bool{false, true} {
		got := frontendDepsPolicy{install: install}.check(context.Background(), "web", dir, dir, "eslint")
		if got.missing || got.note != "" {
			t.Errorf("install=%v: a current tree must just run, got %+v", install, got)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("installed over a current tree: %v", *calls)
	}
}

// A workspace member's dependencies are hoisted to the root: no node_modules
// of its own is not "missing", and there is nothing for --install to do.
func TestFrontendDepsHoistedToWorkspaceRootRuns(t *testing.T) {
	calls := fakeInstaller(t, nil)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "frontends", "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := frontendDepsPolicy{install: true}.check(context.Background(), "web", dir, root, "eslint")
	if got.missing || got.note != "" || len(*calls) != 0 {
		t.Fatalf("hoisted deps: got %+v, installs %v", got, *calls)
	}
}

// Older than its lockfile: the lane runs against what is installed (an mtime
// heuristic never fails a gate), with a note naming the fix. --install
// refreshes it instead.
func TestFrontendDepsStaleRunsWithNoteOrRefreshesUnderInstall(t *testing.T) {
	calls := fakeInstaller(t, nil)
	dir := freshFrontend(t, `{"scripts":{"lint":"eslint ."}}`)
	installed(t, dir)
	newer := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "package-lock.json"), newer, newer); err != nil {
		t.Fatal(err)
	}

	got := frontendDepsPolicy{}.check(context.Background(), "web", dir, dir, "eslint")
	if got.missing || !strings.Contains(got.note, "older than its lockfile") || !strings.Contains(got.note, "forge lint --install") {
		t.Fatalf("stale tree: want a run with a note, got %+v", got)
	}
	if len(*calls) != 0 {
		t.Fatalf("refreshed a stale tree without --install: %v", *calls)
	}

	got = frontendDepsPolicy{install: true, out: io.Discard}.check(context.Background(), "web", dir, dir, "eslint")
	if got.missing || got.note != "" || len(*calls) != 1 {
		t.Fatalf("--install over a stale tree: got %+v, installs %v", got, *calls)
	}
}

// --install installs, and its output goes where the run says: the writer a
// human run names, and nowhere under --quiet.
func TestFrontendDepsInstallOptInWritesOnlyToItsOutput(t *testing.T) {
	calls := fakeInstaller(t, nil)
	dir := freshFrontend(t, `{"scripts":{"lint":"eslint ."}}`)

	var out bytes.Buffer
	got := frontendDepsPolicy{install: true, out: &out}.check(context.Background(), "web", dir, dir, "eslint")
	if got.missing || len(*calls) != 1 {
		t.Fatalf("--install must install and run, got %+v, installs %v", got, *calls)
	}
	if !strings.Contains(out.String(), "Done in") {
		t.Errorf("the install's output did not reach the run's writer: %q", out.String())
	}
	if installOutput(true) != io.Discard {
		t.Error("--quiet must discard a successful install's output")
	}
	if installOutput(false) != os.Stderr {
		t.Error("a human run reports the install on stderr, never stdout (the verdict's stream)")
	}
}

// A failed opted-in install is a lane that could NOT run, carrying the
// package manager's error — the one install output --quiet still shows.
func TestFrontendDepsInstallFailureIsALaneThatCouldNotRun(t *testing.T) {
	fakeInstaller(t, errors.New("ERR_PNPM_FETCH_404 nope"))
	dir := freshFrontend(t, `{"scripts":{"lint":"eslint ."}}`)

	got := frontendDepsPolicy{install: true, out: io.Discard}.check(context.Background(), "web", dir, dir, "eslint")
	if !got.missing || !strings.Contains(got.reason, "ERR_PNPM_FETCH_404") || !strings.Contains(got.reason, "did NOT run") || got.fix == "" {
		t.Fatalf("failed install: got %+v", got)
	}
}

// ── the lanes ──────────────────────────────────────────────────────────────

// The eslint lane reports missing deps as could-not-run, and execs nothing:
// no install, no lint script.
func TestLintFrontendDirNeverInstallsWithoutOptIn(t *testing.T) {
	calls := fakeNPM(t, 0)
	dir := freshFrontend(t, `{"scripts":{"lint":"eslint ."}}`)

	fs, _ := collectFrontendDirJSON(context.Background(), frontendDepsPolicy{}, "web", dir, false, false)
	if len(fs) != 1 || fs[0].Rule != ruleFrontendLintUnavailable {
		t.Fatalf("want the lane's could-not-run finding, got %+v", fs)
	}
	if !strings.Contains(fs[0].FixHint, "forge lint --install") {
		t.Errorf("fix hint does not offer --install: %q", fs[0].FixHint)
	}
	if got := readLog(t, calls); got != "" {
		t.Fatalf("lint ran npm without --install: %q", got)
	}
}

// A frontend with no script to run needs no dependencies: it is not a lane
// that could not run.
func TestLintFrontendDirWithoutLintScriptNeedsNoDeps(t *testing.T) {
	dir := freshFrontend(t, `{"scripts":{}}`)
	fs, gated := collectFrontendDirJSON(context.Background(), frontendDepsPolicy{}, "web", dir, false, false)
	if gated || hasGap(fs) {
		t.Fatalf("no lint script, no deps: want a plain skip, got gated=%v %+v", gated, fs)
	}
}

// With --install, a fresh worktree installs and then lints — the behaviour
// lint had by default before.
func TestLintFrontendDirInstallsMissingDepsThenRunsUnderInstall(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns package-manager / frontend toolchain subprocesses; runs in task test")
	}
	calls := fakeNPM(t, 0)
	dir := freshFrontend(t, `{"scripts":{"lint":"eslint ."}}`)

	deps := frontendDepsPolicy{install: true, out: io.Discard}
	if fs, gated := collectFrontendDirJSON(context.Background(), deps, "web", dir, false, false); gated || hasGap(fs) {
		t.Fatalf("lane failed after a successful install: gated=%v %+v", gated, fs)
	}
	got := readLog(t, calls)
	if !strings.HasPrefix(got, "ci\n") || !strings.Contains(got, "run lint") {
		t.Fatalf("npm calls = %q, want `ci` then `run lint`", got)
	}
}

// If the opted-in install fails, eslint never ran: the lane could NOT run,
// which fails the run by default (lint_skip.go) and names the install command.
func TestLintFrontendDirFailsLoudlyWhenInstallFails(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns package-manager / frontend toolchain subprocesses; runs in task test")
	}
	fakeNPM(t, 1)
	dir := freshFrontend(t, `{"scripts":{"lint":"eslint ."}}`)

	fs, _ := collectFrontendDirJSON(context.Background(), frontendDepsPolicy{install: true, out: io.Discard}, "web", dir, false, false)
	if len(fs) != 1 || fs[0].Rule != ruleFrontendLintUnavailable {
		t.Fatalf("install failure must be a lane that could NOT run, got %+v", fs)
	}
	if !strings.Contains(fs[0].Message, "did NOT run") || !strings.Contains(fs[0].FixHint, "npm ") {
		t.Errorf("want the reason and the install command, got %+v", fs[0])
	}
	rc := &lintRunCtx{}
	if v, _ := rc.judgeGap("frontend lint", true); v != gapFails {
		t.Errorf("a gating lane that could not run must fail the run, got verdict %v", v)
	}
}

func TestTypecheckFrontendNeverInstallsWithoutOptIn(t *testing.T) {
	calls := fakeNPM(t, 0)
	dir := freshFrontend(t, `{"scripts":{"typecheck":"tsc --noEmit"}}`)

	res := typecheckFrontend(context.Background(), frontendDepsPolicy{}, frontendTarget{name: "web", dir: dir}, dir)
	if res.status != typecheckUnavailable {
		t.Fatalf("status = %v, want unavailable (lint installs nothing)", res.status)
	}
	if !strings.Contains(res.reason, "did NOT run") || !strings.Contains(res.fixHint, "forge lint --install") {
		t.Errorf("want the reason and the fix, got reason=%q fix=%q", res.reason, res.fixHint)
	}
	if got := readLog(t, calls); got != "" {
		t.Fatalf("typecheck ran npm without --install: %q", got)
	}
}

func TestTypecheckFrontendInstallsMissingDepsUnderInstall(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns package-manager / frontend toolchain subprocesses; runs in task test")
	}
	calls := fakeNPM(t, 0)
	dir := freshFrontend(t, `{"scripts":{"typecheck":"tsc --noEmit"}}`)

	res := typecheckFrontend(context.Background(), frontendDepsPolicy{install: true, out: io.Discard}, frontendTarget{name: "web", dir: dir}, dir)
	if res.status != typecheckPassed {
		t.Fatalf("status = %v (%s%s), want passed after install", res.status, res.reason, res.output)
	}
	if got := readLog(t, calls); !strings.HasPrefix(got, "ci\n") {
		t.Fatalf("npm calls = %q, want `ci` first", got)
	}
}

func TestTypecheckFrontendFailsWhenInstallFails(t *testing.T) {
	fakeNPM(t, 1)
	dir := freshFrontend(t, `{"scripts":{"typecheck":"tsc --noEmit"}}`)

	res := typecheckFrontend(context.Background(), frontendDepsPolicy{install: true, out: io.Discard}, frontendTarget{name: "web", dir: dir}, dir)
	// Could NOT run — which fails the gating lane (lint_skip.go) — not a
	// type error, and never green.
	if res.status != typecheckUnavailable {
		t.Fatalf("status = %v, want unavailable (the compiler never ran)", res.status)
	}
	if !strings.Contains(res.reason, "did NOT run") || res.fixHint == "" {
		t.Errorf("want the reason and the fix, got reason=%q fix=%q", res.reason, res.fixHint)
	}
}

// The whole run under --quiet: the missing lane fails the verdict, by name,
// with the install command and --install; an opted-in install that succeeds
// prints nothing.
func TestQuietRunOverMissingDepsNamesTheLaneAndPrintsNoInstall(t *testing.T) {
	cfg := dashboardWithoutDeps(t)
	steps := []linterStep{findStep(t, "frontend lint")}

	var out strings.Builder
	err := renderLintOutcomes(&out, collectLintOutcomesFrom(&lintRunCtx{ctx: context.Background(), cfg: cfg}, steps), true, nil)
	if err == nil || !strings.Contains(err.Error(), "forge lint --install") {
		t.Fatalf("verdict must fail naming --install, got err=%v\n%s", err, out.String())
	}

	calls := fakeInstaller(t, nil)
	out.Reset()
	rc := &lintRunCtx{ctx: context.Background(), cfg: cfg, install: true, installOut: installOutput(true)}
	_ = renderLintOutcomes(&out, collectLintOutcomesFrom(rc, steps), true, nil)
	if len(*calls) != 1 {
		t.Fatalf("--install did not install: %v", *calls)
	}
	if strings.Contains(out.String(), "Done in") {
		t.Errorf("--quiet printed the install's output:\n%s", out.String())
	}
}

// hasGap reports whether a lane's findings include its could-not-run headline.
func hasGap(fs []lintJSONFinding) bool {
	for _, f := range fs {
		if isGapRule(f.Rule) {
			return true
		}
	}
	return false
}
