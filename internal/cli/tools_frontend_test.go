package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// `forge tools install` must never mutate a frontend's package.json or
// lockfile. It used to run `npm install --save-dev @bufbuild/protoc-gen-es`
// in every frontend on --force (which the scaffolded verify-generated job
// passes), and in any frontend whose node_modules was not populated yet. On
// a Linux runner that re-resolved a lockfile a macOS npm had written and
// dropped its "libc" entries; `forge ci verify-generated` then reported the
// user-owned package-lock.json as generated-code drift (houndersclub
// 962c35c). Committing either OS's lockfile only moved the failure to the
// other OS.
//
// The plugin is a devDependency the scaffold declares. The frontend's own
// `npm ci` installs it; forge only checks that it is declared.

const toolsTestLockfile = `{
  "name": "web",
  "lockfileVersion": 3,
  "packages": {
    "node_modules/@esbuild/linux-x64": {
      "version": "0.25.0",
      "libc": ["glibc"]
    }
  }
}
`

// toolsInstallFixture is a one-frontend forge project plus a PATH holding
// only fakes: `go` and `npm` that record their argv, and stub binaries for
// every Go tool so the install's on-PATH check passes. The fake npm does
// what the Linux runner's npm did — it rewrites package-lock.json.
type toolsInstallFixture struct {
	root, feDir string
	npmLog      string
	pkgJSON     []byte
}

func newToolsInstallFixture(t *testing.T, pkgJSON string) toolsInstallFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tools are POSIX shell scripts")
	}
	root := t.TempDir()
	feDir := filepath.Join(root, "frontends", "web")
	bin := filepath.Join(root, ".fakebin")
	npmLog := filepath.Join(root, "npm.log")
	for _, d := range []string{feDir, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "forge.yaml"), "name: demo\nmodule_path: github.com/demo/demo\n"+
		"frontends:\n  - name: web\n    type: nextjs\n    path: frontends/web\n", 0o644)
	write(filepath.Join(feDir, "buf.gen.yaml"),
		"version: v2\nplugins:\n  - local: ./frontends/web/node_modules/.bin/protoc-gen-es\n    out: frontends/web/src/gen\n", 0o644)
	write(filepath.Join(feDir, "package.json"), pkgJSON, 0o644)
	write(filepath.Join(feDir, "package-lock.json"), toolsTestLockfile, 0o644)

	write(filepath.Join(bin, "npm"), "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> '"+npmLog+"'\n"+
		"printf '{\"lockfileVersion\":3,\"rewritten\":\"by npm on linux\"}\\n' > package-lock.json\n", 0o755)
	write(filepath.Join(bin, "go"), "#!/bin/sh\nexit 0\n", 0o755)
	for _, tool := range requiredProtoTools {
		write(filepath.Join(bin, tool.Binary), "#!/bin/sh\nexit 0\n", 0o755)
	}
	t.Setenv("PATH", bin)
	t.Chdir(root)

	return toolsInstallFixture{root: root, feDir: feDir, npmLog: npmLog, pkgJSON: []byte(pkgJSON)}
}

func (f toolsInstallFixture) run(t *testing.T, args ...string) error {
	t.Helper()
	cmd := newToolsInstallCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd.Execute()
}

// assertUntouched fails when npm ran at all, or when either user-owned
// manifest changed by a single byte.
func (f toolsInstallFixture) assertUntouched(t *testing.T) {
	t.Helper()
	if log, err := os.ReadFile(f.npmLog); err == nil {
		t.Errorf("forge tools install ran npm, which may rewrite a user-owned lockfile:\n  npm %s",
			strings.ReplaceAll(strings.TrimSpace(string(log)), "\n", "\n  npm "))
	}
	for name, want := range map[string][]byte{
		"package.json":      f.pkgJSON,
		"package-lock.json": []byte(toolsTestLockfile),
	} {
		got, err := os.ReadFile(filepath.Join(f.feDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("frontends/web/%s changed — forge does not own it:\n--- want\n%s\n--- got\n%s", name, want, got)
		}
	}
}

const declaredPackageJSON = `{
  "name": "web",
  "devDependencies": {
    "@bufbuild/protoc-gen-es": "^2.5.0"
  }
}
`

func TestToolsInstall_DeclaredTSPluginNeverRunsNPM(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed bool
		args      []string
	}{
		// The scaffolded verify-generated job: `npm ci` has not run yet or
		// already has, and --force is passed for the Go plugins.
		{name: "installed, --force", installed: true, args: []string{"--force"}},
		// A fresh clone before `npm ci`: declared, not yet installed. The
		// frontend's own install puts it there; forge must not.
		{name: "declared but not installed", installed: false},
		{name: "declared but not installed, --force", installed: false, args: []string{"--force"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newToolsInstallFixture(t, declaredPackageJSON)
			if tc.installed {
				writeTSPluginBin(t, f.feDir)
			}
			if err := f.run(t, tc.args...); err != nil {
				t.Fatalf("forge tools install failed over a frontend that declares the plugin: %v", err)
			}
			f.assertUntouched(t)
		})
	}
}

// A frontend that does not declare the plugin cannot have its TypeScript
// stubs regenerated from a clean checkout. forge refuses with the edit to
// make, rather than making it.
func TestToolsInstall_UndeclaredTSPluginFailsWithRunbook(t *testing.T) {
	f := newToolsInstallFixture(t, "{\n  \"name\": \"web\",\n  \"devDependencies\": {}\n}\n")
	err := f.run(t, "--force")
	if err == nil {
		t.Fatal("forge tools install succeeded although frontends/web declares no @bufbuild/protoc-gen-es")
	}
	for _, want := range []string{
		"frontends/web/package.json",
		`"@bufbuild/protoc-gen-es": "` + frontendTSPluginRange + `"`,
		"devDependencies",
		"package-lock.json",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("runbook does not mention %q:\n%v", want, err)
		}
	}
	f.assertUntouched(t)
}

// verify-generated must not blame `forge generate` for a file an earlier
// step changed. The houndersclub report listed package-lock.json under
// "generated code is out of date" with nothing saying forge generate never
// touched it.
func TestCheckTreeCleanBeforeRegenerate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	lock := filepath.Join(root, "frontends", "web", "package-lock.json")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte(toolsTestLockfile), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "init")
	t.Chdir(root)
	ctx := context.Background()

	if err := checkTreeCleanBeforeRegenerate(ctx, io.Discard); err != nil {
		t.Fatalf("a clean checkout was refused: %v", err)
	}

	if err := os.WriteFile(lock, []byte(`{"rewritten":"by npm on linux"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var report bytes.Buffer
	err := checkTreeCleanBeforeRegenerate(ctx, &report)
	if err == nil {
		t.Fatal("regenerate would run over a lockfile an earlier step rewrote, and report it as generated-code drift")
	}
	if strings.Contains(err.Error(), "out of date") {
		t.Errorf("error still reads as generated-code drift: %v", err)
	}
	for _, want := range []string{"frontends/web/package-lock.json", "before `forge generate` ran", "an earlier step"} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("report does not say %q:\n%s", want, report.String())
		}
	}
}

// The runbook's range must be the one forge scaffolds, or following it
// would put a project on a plugin version its own scaffold never uses.
func TestFrontendTSPluginRangeMatchesTemplates(t *testing.T) {
	want := `"` + frontendTSPluginPackage + `": "` + frontendTSPluginRange + `"`
	for _, kind := range []string{"nextjs", "vite-spa", "react-native"} {
		path := filepath.Join("..", "templates", "frontend", kind, "package.json.tmpl")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("%s does not declare %s", path, want)
		}
	}
}
