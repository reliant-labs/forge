package lint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestMain keeps the pre-existing "deps missing" tests hermetic: the lanes now
// install missing node_modules, and those tests assert the not-installed
// classification, not a real `npm ci`. Tests of the install itself clear it.
func TestMain(m *testing.M) {
	os.Setenv("FORGE_SKIP_NPM_INSTALL", "1")
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

// A fresh worktree has no node_modules. The eslint lane must install and then
// run — not report "could not run", which reads as a green.
func TestLintFrontendDirInstallsMissingDepsThenRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns package-manager / frontend toolchain subprocesses; runs in task test")
	}
	calls := fakeNPM(t, 0)
	dir := freshFrontend(t, `{"scripts":{"lint":"eslint ."}}`)

	if err := lintFrontendDir(context.Background(), "web", dir, "", false, false); err != nil {
		t.Fatalf("lane failed after a successful install: %v", err)
	}
	got := readLog(t, calls)
	if !strings.HasPrefix(got, "ci\n") || !strings.Contains(got, "run lint") {
		t.Fatalf("npm calls = %q, want `ci` then `run lint`", got)
	}
}

// If the install itself fails, the lane fails loudly: neither a pass nor the
// soft "unavailable" classification.
func TestLintFrontendDirFailsLoudlyWhenInstallFails(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns package-manager / frontend toolchain subprocesses; runs in task test")
	}
	fakeNPM(t, 1)
	dir := freshFrontend(t, `{"scripts":{"lint":"eslint ."}}`)

	err := lintFrontendDir(context.Background(), "web", dir, "", false, false)
	if err == nil {
		t.Fatal("lane passed although dependencies could not be installed")
	}
	var unavail *laneUnavailableError
	if errors.As(err, &unavail) {
		t.Fatalf("install failure was downgraded to unavailable: %v", err)
	}
	if !strings.Contains(err.Error(), "did NOT run") {
		t.Errorf("error does not say the lane did not run: %v", err)
	}
}

func TestTypecheckFrontendInstallsMissingDeps(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns package-manager / frontend toolchain subprocesses; runs in task test")
	}
	calls := fakeNPM(t, 0)
	dir := freshFrontend(t, `{"scripts":{"typecheck":"tsc --noEmit"}}`)

	res := typecheckFrontend(context.Background(), frontendTarget{name: "web", dir: dir}, dir)
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

	res := typecheckFrontend(context.Background(), frontendTarget{name: "web", dir: dir}, dir)
	if res.status != typecheckFailed {
		t.Fatalf("status = %v, want failed (a lane that cannot run must not be green or merely a warning)", res.status)
	}
	if !strings.Contains(res.output, "did NOT run") {
		t.Errorf("output does not say the typecheck did not run: %q", res.output)
	}
}
