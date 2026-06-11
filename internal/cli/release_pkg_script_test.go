// File: internal/cli/release_pkg_script_test.go
//
// Exercises scripts/release-pkg.sh (the pkg/vX.Y.Z submodule-tag release
// gate) in --dry-run mode against throwaway fixture repos, covering each
// validation branch: version shape, module identity, dirty tree,
// duplicate tag, standalone-build failure, and the happy path. The
// script is the chokepoint that keeps broken pkg versions from being
// tagged, so its refusal logic gets the same test discipline as Go code.
//
// These tests shell out to bash + git + go; they skip when bash is
// unavailable (never the case on the supported dev platforms).
package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// releasePkgScriptPath walks up from the package dir to the repo root's
// scripts/release-pkg.sh.
func releasePkgScriptPath(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	p := filepath.Join(cwd, "..", "..", "scripts", "release-pkg.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("release-pkg.sh not found at %s: %v", p, err)
	}
	return p
}

// newPkgFixtureRepo builds a minimal git repo shaped like the forge repo
// from the script's point of view: pkg/go.mod + one buildable Go file,
// committed.
func newPkgFixtureRepo(t *testing.T, module string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	pkgDir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module " + module + "\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package svcerr\n\n// OK is a placeholder.\nconst OK = true\n"
	if err := os.MkdirAll(filepath.Join(pkgDir, "svcerr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "svcerr", "svcerr.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"add", "."},
		{"commit", "-q", "-m", "fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

// runReleaseScript invokes the script and returns combined output + exit
// error.
func runReleaseScript(t *testing.T, repo string, args ...string) (string, error) {
	t.Helper()
	script := releasePkgScriptPath(t)
	full := append([]string{script, "--repo", repo}, args...)
	cmd := exec.Command("bash", full...)
	// The standalone-build step runs `go build` in the fixture module;
	// inherit the environment so GOPATH/GOCACHE resolution works.
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestReleasePkgScript_DryRunHappyPath(t *testing.T) {
	repo := newPkgFixtureRepo(t, "github.com/reliant-labs/forge/pkg")
	out, err := runReleaseScript(t, repo, "--dry-run", "v0.1.0")
	if err != nil {
		t.Fatalf("dry-run failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"DRY RUN: all validations passed",
		"would create annotated tag pkg/v0.1.0",
		"git push origin pkg/v0.1.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Dry run must not create the tag.
	cmd := exec.Command("git", "tag", "-l", "pkg/v0.1.0")
	cmd.Dir = repo
	tags, _ := cmd.Output()
	if strings.TrimSpace(string(tags)) != "" {
		t.Errorf("dry-run created tag: %q", tags)
	}
}

func TestReleasePkgScript_RejectsBadVersions(t *testing.T) {
	repo := newPkgFixtureRepo(t, "github.com/reliant-labs/forge/pkg")
	for _, bad := range []string{"0.1.0", "pkg/v0.1.0", "v1.2", "latest", "v1.2.3+meta"} {
		out, err := runReleaseScript(t, repo, "--dry-run", bad)
		if err == nil {
			t.Errorf("version %q: expected rejection, got success:\n%s", bad, out)
			continue
		}
		if !strings.Contains(out, "version must look like vX.Y.Z") {
			t.Errorf("version %q: unexpected error output:\n%s", bad, out)
		}
	}
}

func TestReleasePkgScript_RejectsWrongModule(t *testing.T) {
	repo := newPkgFixtureRepo(t, "github.com/someone-else/pkg")
	out, err := runReleaseScript(t, repo, "--dry-run", "v0.1.0")
	if err == nil {
		t.Fatalf("expected wrong-module rejection, got success:\n%s", out)
	}
	if !strings.Contains(out, "does not declare module github.com/reliant-labs/forge/pkg") {
		t.Errorf("unexpected error output:\n%s", out)
	}
}

func TestReleasePkgScript_RejectsDirtyPkgTree(t *testing.T) {
	repo := newPkgFixtureRepo(t, "github.com/reliant-labs/forge/pkg")
	if err := os.WriteFile(filepath.Join(repo, "pkg", "svcerr", "dirty.go"), []byte("package svcerr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runReleaseScript(t, repo, "--dry-run", "v0.1.0")
	if err == nil {
		t.Fatalf("expected dirty-tree rejection, got success:\n%s", out)
	}
	if !strings.Contains(out, "uncommitted changes") {
		t.Errorf("unexpected error output:\n%s", out)
	}
}

func TestReleasePkgScript_RejectsExistingTag(t *testing.T) {
	repo := newPkgFixtureRepo(t, "github.com/reliant-labs/forge/pkg")
	cmd := exec.Command("git", "tag", "pkg/v0.1.0")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git tag: %v\n%s", err, out)
	}
	out, err := runReleaseScript(t, repo, "--dry-run", "v0.1.0")
	if err == nil {
		t.Fatalf("expected existing-tag rejection, got success:\n%s", out)
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("unexpected error output:\n%s", out)
	}
}

func TestReleasePkgScript_RejectsBrokenStandaloneBuild(t *testing.T) {
	repo := newPkgFixtureRepo(t, "github.com/reliant-labs/forge/pkg")
	// Commit a compile error so the tree is clean but the standalone
	// build gate fails.
	if err := os.WriteFile(filepath.Join(repo, "pkg", "svcerr", "broken.go"), []byte("package svcerr\n\nfunc broken() { undefinedSymbol() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "break build"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	out, err := runReleaseScript(t, repo, "--dry-run", "v0.1.0")
	if err == nil {
		t.Fatalf("expected standalone-build rejection, got success:\n%s", out)
	}
	if !strings.Contains(out, "validating pkg module builds standalone") {
		t.Errorf("expected the build-validation banner before failure:\n%s", out)
	}
}
