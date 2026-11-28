package frontenddeps

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func evalDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func nodeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if _, ok := files["package.json"]; !ok {
		files["package.json"] = `{"name":"fe"}`
	}
	for name, body := range files {
		writeFileAt(t, dir, name, body)
	}
	return dir
}

func TestEnsureTo_InstallsAStaleTreeAndStampsIt(t *testing.T) {
	pm := installFakePM(t, "npm")
	dir := nodeProject(t, map[string]string{"package-lock.json": "{}"})
	var out bytes.Buffer

	if err := EnsureTo(context.Background(), &out, "[up]", "web", dir, "npm", true); err != nil {
		t.Fatalf("EnsureTo: %v", err)
	}
	calls := pm.calls(t)
	if len(calls) != 1 || calls[0][0] != "npm" || !reflect.DeepEqual(calls[0][2:], []string{"ci"}) {
		t.Fatalf("calls = %v, want one `npm ci` (frozen, with a lockfile)", calls)
	}
	if got := evalDir(t, calls[0][1]); got != evalDir(t, dir) {
		t.Errorf("the install ran in %q, want the frontend dir %q", got, dir)
	}
	if !strings.Contains(out.String(), "[up] web: node_modules missing/stale — running `npm ci`") {
		t.Errorf("progress = %q", out.String())
	}
	if _, err := os.Stat(installStamp(dir)); err != nil {
		t.Errorf("a completed install must leave its stamp: %v", err)
	}

	// The stamp is what makes the second run a no-op.
	past := time.Now().Add(-time.Hour)
	for _, f := range []string{"package.json", "package-lock.json"} {
		if err := os.Chtimes(filepath.Join(dir, f), past, past); err != nil {
			t.Fatal(err)
		}
	}
	if Stale(dir) {
		t.Fatal("a tree stamped after its manifests must be current")
	}
	if err := EnsureTo(context.Background(), &out, "[up]", "web", dir, "npm", true); err != nil {
		t.Fatal(err)
	}
	if got := len(pm.calls(t)); got != 1 {
		t.Errorf("a current tree was reinstalled (%d calls)", got)
	}
}

func TestEnsureTo_Skips(t *testing.T) {
	pm := installFakePM(t, "npm")
	ctx := context.Background()

	t.Run("an empty directory", func(t *testing.T) {
		if err := EnsureTo(ctx, nil, "p", "n", "", "npm", false); err != nil {
			t.Error(err)
		}
	})
	t.Run("no package.json is not a node project", func(t *testing.T) {
		if err := EnsureTo(ctx, nil, "p", "n", t.TempDir(), "npm", false); err != nil {
			t.Error(err)
		}
	})
	t.Run("FORGE_SKIP_NPM_INSTALL", func(t *testing.T) {
		t.Setenv("FORGE_SKIP_NPM_INSTALL", "1")
		if err := EnsureTo(ctx, nil, "p", "n", nodeProject(t, map[string]string{}), "npm", false); err != nil {
			t.Error(err)
		}
	})
	if got := pm.calls(t); len(got) != 0 {
		t.Errorf("an install ran for a project that must be skipped: %v", got)
	}
}

func TestEnsureTo_FailureCarriesThePackageManagerOutput(t *testing.T) {
	pm := installFakePM(t, "npm")
	t.Setenv("FAKEPM_EXIT", "1")
	t.Setenv("FAKEPM_STDERR", "npm error 404 Not Found - GET https://registry.npmjs.org/nope\n")
	dir := nodeProject(t, map[string]string{})
	var out bytes.Buffer

	err := EnsureTo(context.Background(), &out, "[up]", "web", dir, "npm", false)
	if err == nil {
		t.Fatal("a failing install must fail")
	}
	if !strings.Contains(err.Error(), "install deps in "+dir) || !strings.Contains(err.Error(), "404 Not Found") {
		t.Errorf("error %q must name the directory and carry the package manager's output", err)
	}
	if got := len(pm.calls(t)); got != 1 {
		t.Errorf("a genuine dependency failure was retried (%d calls)", got)
	}
	if _, statErr := os.Stat(installStamp(dir)); statErr == nil {
		t.Error("a failed install must not be stamped complete")
	}
	if !strings.Contains(out.String(), "404 Not Found") {
		t.Errorf("the output sink did not receive the package manager's output: %q", out.String())
	}
}

func TestEnsureTo_RetriesOnePackageManagerInternalError(t *testing.T) {
	pm := installFakePM(t, "npm")
	t.Setenv("FAKEPM_FAIL_FIRST", "1")
	t.Setenv("FAKEPM_FAIL_MSG", "npm error Exit handler never called!\n")
	dir := nodeProject(t, map[string]string{})
	var out bytes.Buffer

	if err := EnsureTo(context.Background(), &out, "[up]", "web", dir, "npm", false); err != nil {
		t.Fatalf("a transient failure must be retried and recover: %v", err)
	}
	if got := len(pm.calls(t)); got != 2 {
		t.Errorf("calls = %d, want the failure and exactly one retry", got)
	}
	if !strings.Contains(out.String(), "hit a package-manager internal error — retrying once") {
		t.Errorf("the retry must be announced: %q", out.String())
	}
	if _, err := os.Stat(installStamp(dir)); err != nil {
		t.Errorf("the recovered install must be stamped: %v", err)
	}
}

func TestEnsureTo_RetriesOnlyOnce(t *testing.T) {
	pm := installFakePM(t, "npm")
	t.Setenv("FAKEPM_FAIL_FIRST", "5")
	t.Setenv("FAKEPM_FAIL_MSG", "ECONNRESET\n")
	err := EnsureTo(context.Background(), nil, "[up]", "web", nodeProject(t, map[string]string{}), "npm", false)
	if err == nil || !strings.Contains(err.Error(), "ECONNRESET") {
		t.Fatalf("err = %v, want the second failure", err)
	}
	if got := len(pm.calls(t)); got != 2 {
		t.Errorf("calls = %d, want exactly two attempts", got)
	}
}

func TestEnsureTo_UnstartableRunnerIsNotRetried(t *testing.T) {
	pm := installFakePM(t, "npm")
	t.Setenv("FAKEPM_EXIT", "1")
	t.Setenv("FAKEPM_STDERR", "Error: spawn /tools/pnpm EACCES ECONNRESET\n")
	err := EnsureTo(context.Background(), nil, "[up]", "web", nodeProject(t, map[string]string{}), "npm", false)
	if !errors.Is(err, ErrRunnerUnusable) {
		t.Fatalf("err = %v, want ErrRunnerUnusable", err)
	}
	if got := len(pm.calls(t)); got != 1 {
		t.Errorf("an executable that will not start was retried (%d calls)", got)
	}
}

func TestEnsureTo_PnpmWarnsWhenItCopiedFromTheStore(t *testing.T) {
	installFakePM(t, "pnpm")
	t.Setenv("FAKEPM_STDOUT", "Packages are copied from the content-addressable store to the virtual store.\n")
	dir := nodeProject(t, map[string]string{"pnpm-lock.yaml": "lockfileVersion: '9.0'\n"})
	var out bytes.Buffer
	if err := EnsureTo(context.Background(), &out, "[up]", "web", dir, "pnpm", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "pnpm COPIED every package") {
		t.Errorf("the copy fallback must be surfaced as a warning: %q", out.String())
	}
}

func TestEnsureTo_ProxyCapsConcurrency(t *testing.T) {
	pm := installFakePM(t, "npm")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	var out bytes.Buffer
	if err := EnsureTo(context.Background(), &out, "[up]", "web", nodeProject(t, map[string]string{}), "npm", false); err != nil {
		t.Fatal(err)
	}
	calls := pm.calls(t)
	if len(calls) != 1 || !reflect.DeepEqual(calls[0][2:], []string{"install", "--maxsockets=8"}) {
		t.Errorf("calls = %v, want the install capped at 8 sockets behind a proxy", calls)
	}
	if !strings.Contains(out.String(), "proxy detected") {
		t.Errorf("the cap must be announced: %q", out.String())
	}
}

func TestEnsure_WritesToTheTerminal(t *testing.T) {
	installFakePM(t, "npm")
	dir := nodeProject(t, map[string]string{})
	if err := Ensure(context.Background(), "[up]", "web", dir, "npm", false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := os.Stat(installStamp(dir)); err != nil {
		t.Errorf("Ensure must install and stamp: %v", err)
	}
}

func TestInstall_RunsEvenWhenCurrent(t *testing.T) {
	pm := installFakePM(t, "npm")
	dir := nodeProject(t, map[string]string{"package-lock.json": "{}"})
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Install(context.Background(), dir, "npm", true); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if calls := pm.calls(t); len(calls) != 1 || calls[0][2] != "ci" {
		t.Errorf("calls = %v, want `npm ci` regardless of staleness", calls)
	}
	t.Setenv("FAKEPM_EXIT", "2")
	if err := Install(context.Background(), dir, "npm", false); err == nil {
		t.Error("a failing install must be reported")
	}
}

func TestInstallArgs(t *testing.T) {
	tests := []struct {
		name   string
		files  map[string]string
		runner string
		frozen bool
		want   []string
	}{
		{"npm dev install", nil, "npm", false, []string{"install"}},
		{"npm frozen with a lockfile", map[string]string{"package-lock.json": "{}"}, "npm", true, []string{"ci"}},
		{"npm frozen without a lockfile falls back to install", nil, "npm", true, []string{"install"}},
		{"pnpm dev install must not freeze even in CI", nil, "pnpm", false, []string{"install", "--no-frozen-lockfile"}},
		{"pnpm frozen", nil, "pnpm", true, []string{"install", "--frozen-lockfile"}},
		{"yarn classic frozen", nil, "yarn", true, []string{"install", "--frozen-lockfile"}},
		{"yarn berry frozen is immutable", map[string]string{".yarnrc.yml": "nodeLinker: node-modules\n"}, "yarn", true, []string{"install", "--immutable"}},
		{"yarn dev install", nil, "yarn", false, []string{"install"}},
		{"an unknown manager", nil, "bun", true, []string{"install"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := tt.files
			if files == nil {
				files = map[string]string{}
			}
			dir := nodeProject(t, files)
			if got := installArgs(tt.runner, dir, tt.frozen); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("installArgs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOutput(t *testing.T) {
	var buf bytes.Buffer
	o := output{w: &buf}
	o.printf("hello %s\n", "world")
	o.warn("careful")
	stdout, stderr, end := o.step("label")
	stdout.Write([]byte("out\n"))
	stderr.Write([]byte("err\n"))
	end(nil)
	if got, want := buf.String(), "hello world\ncareful\nout\nerr\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}

	var sink bytes.Buffer
	lw := &lockedWriter{mu: new(sync.Mutex), w: &sink}
	if n, err := lw.Write([]byte("abc")); n != 3 || err != nil || sink.String() != "abc" {
		t.Errorf("lockedWriter.Write = (%d, %v), sink %q", n, err, sink.String())
	}
}

func TestOutput_TerminalDefaults(t *testing.T) {
	// The zero value writes to the process's stdout/stderr; capture them.
	capture := func(f **os.File, fn func()) string {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		old := *f
		*f = w
		fn()
		_ = w.Close()
		*f = old
		b, _ := io.ReadAll(r)
		return string(b)
	}
	var o output
	if got := capture(&os.Stdout, func() { o.printf("to %s\n", "stdout") }); got != "to stdout\n" {
		t.Errorf("printf wrote %q to stdout", got)
	}
	if got := capture(&os.Stderr, func() { o.warn("to stderr") }); got != "to stderr\n" {
		t.Errorf("warn wrote %q to stderr", got)
	}
	stdout, stderr, end := o.step("x")
	if stdout == nil || stderr == nil || end == nil {
		t.Fatal("step must return writers and a closer")
	}
	end(nil)
}

func TestInstallStampPerManager(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]string{
		"pnpm": filepath.Join(dir, "node_modules", ".modules.yaml"),
		"yarn": filepath.Join(dir, "node_modules", ".yarn-integrity"),
		"bun":  "",
	}
	for runner, want := range tests {
		if got := InstallStamp(dir, runner); got != want {
			t.Errorf("InstallStamp(%s) = %q, want %q", runner, got, want)
		}
	}
	if got := InstallStamp(dir, "npm"); !strings.HasSuffix(got, filepath.Join("node_modules", ".package-lock.json")) {
		t.Errorf("InstallStamp(npm) = %q", got)
	}
}

func TestLockfileName(t *testing.T) {
	for runner, want := range map[string]string{"npm": "package-lock.json", "pnpm": "pnpm-lock.yaml", "yarn": "yarn.lock", "bun": ""} {
		if got := LockfileName(runner); got != want {
			t.Errorf("LockfileName(%s) = %q, want %q", runner, got, want)
		}
	}
}

func TestExecArgv(t *testing.T) {
	installFakePM(t, "pnpm")
	dir := t.TempDir()
	got := ExecArgv(dir, "pnpm", "run", "build")
	if len(got) != 3 || filepath.Base(got[0]) != "pnpm" && got[0] != "pnpm" || got[1] != "run" || got[2] != "build" {
		t.Errorf("ExecArgv = %v, want pnpm run build", got)
	}
}

func TestDirExists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !dirExists(dir) || dirExists(file) || dirExists(filepath.Join(dir, "absent")) {
		t.Error("dirExists must be true only for an existing directory")
	}
}
