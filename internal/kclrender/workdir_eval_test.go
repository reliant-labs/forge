//go:build cgo

package kclrender_test

// workdir_eval_test.go pins that workDir decides where a project's KCL reads
// FILES from, independent of the caller's process cwd.
//
// This was broken for every render path, `forge env render` included. kpm's
// client.WithWorkDir decides where kpm resolves the PACKAGE from, and it was
// the only directory Run set — but the KCL runtime's `file.read` resolves a
// relative path against the real process cwd, which nothing set. So a project
// whose KCL reads a file by project-relative path rendered correctly when
// invoked from the project root and failed with "No such file or directory"
// from anywhere else, naming a path that plainly exists.
//
// The shape is not hypothetical: control-plane's deploy/kcl/lib/barman_plugin.k
// does `file.read("deploy/cnpg/plugin-barman-cloud.yaml")`, and its
// lib/platform_local.k renders that plugin into the local platform set. Every
// shell script that read one of those files cd'd to the right directory first,
// which is how the gap stayed hidden — and `forge -C <dir> env render` had no
// such cd to hide behind.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/kclrender"
	"github.com/reliant-labs/forge/internal/kclvendor"
)

// TestRunResolvesFileReadAgainstWorkDirNotTheCallerCwd renders the same
// project from two different process cwds and requires the same answer.
func TestRunResolvesFileReadAgainstWorkDirNotTheCallerCwd(t *testing.T) {
	t.Cleanup(kclvendor.SetCacheDirForTest(t.TempDir()))
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"deploy/kcl/kcl.mod":      "[package]\nname = \"workdir_probe\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\n",
		"deploy/kcl/dev/main.k":   "import forge\nimport file\n\npinned = file.read(\"deploy/vendored/pin.txt\")\n",
		"deploy/vendored/pin.txt": "v0.15.0",
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	envDir := filepath.Join(dir, "deploy", "kcl", "dev")

	// An UNRELATED directory, which is what a caller using -C stands in.
	// Deliberately not a subdirectory of the project: the failing case was
	// any cwd that is not the project root.
	for _, cwd := range []string{dir, t.TempDir(), filepath.Join(dir, "deploy", "kcl")} {
		t.Chdir(cwd)
		out, err := kclrender.Run(dir, envDir, []string{"env=dev"})
		if err != nil {
			t.Fatalf("render from cwd %s: a project-relative file.read must resolve against workDir: %v", cwd, err)
		}
		if !strings.Contains(string(out), "v0.15.0") {
			t.Errorf("render from cwd %s did not read the file:\n%s", cwd, out)
		}
	}
}

// TestRunRestoresTheCallerCwd: the render's chdir is an implementation detail
// and must not leak. A command that renders and then resolves a relative path
// of its own — or a test that renders and then reads a fixture — would
// otherwise silently read from the project instead.
func TestRunRestoresTheCallerCwd(t *testing.T) {
	t.Cleanup(kclvendor.SetCacheDirForTest(t.TempDir()))
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"deploy/kcl/kcl.mod":    "[package]\nname = \"cwd_probe\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\n",
		"deploy/kcl/dev/main.k": "import forge\n\nok = forge.Bundle is not None\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	caller := t.TempDir()
	t.Chdir(caller)
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kclrender.Run(dir, filepath.Join(dir, "deploy", "kcl", "dev"), []string{"env=dev"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("render left the process in %s; it must restore %s", after, before)
	}

	// And a render that FAILS must restore it too — the defer, not the happy
	// path. A refused render is the common case in a validation loop, so a
	// leak here would be the one that actually bit.
	_, err = kclrender.Run(dir, filepath.Join(dir, "deploy", "kcl", "nope"), nil)
	if err == nil {
		t.Fatal("render of a nonexistent env succeeded")
	}
	if got, _ := os.Getwd(); got != before {
		t.Errorf("a FAILED render left the process in %s; it must restore %s", got, before)
	}
}
