// File: internal/cli/release_forge_script_test.go
//
// Exercises scripts/release-forge.sh — the one-command release gate that
// tags BOTH github.com/reliant-labs/forge (vX.Y.Z) and its pkg submodule
// (pkg/vX.Y.Z) at a single commit.
//
// The tests that matter most here are not the refusals but the two
// invariants the old two-commit flow could not hold:
//
//   - the go.sum assertion. An in-workspace `go build ./...` passes with NO
//     forge/pkg hashes in go.sum, because go.work resolves pkg from disk. The
//     gap only surfaces for a consumer, after the release is public. The
//     script resolves the hashes from a local clone before the tag is pushed;
//     TestReleaseForgeScript_DryRunPopulatesGoSum pins that it really does.
//   - a dry run leaving NO trace. The script edits five files in place, so a
//     failed or dry run must restore the tree exactly — this checkout is
//     routinely shared with other agents, and the obvious cleanup verb
//     (`git checkout -- <path>`) is the destructive one we must never use.
//
// These tests shell out to bash + git + go; they skip when bash is
// unavailable (never the case on the supported dev platforms). The
// resolution step runs a real `go mod download` against a local bare clone,
// which is why they are skipped in -short mode.
package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func releaseForgeScriptPath(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	p := filepath.Join(cwd, "..", "..", "scripts", "release-forge.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("release-forge.sh not found at %s: %v", p, err)
	}
	return p
}

// gitIn runs a git command in dir and fails the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// gitOut runs a git command in dir and returns trimmed stdout.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

// newForgeFixtureRepo builds a minimal repo shaped like the forge repo from
// the script's point of view: a root module requiring its own ./pkg
// submodule, the three version files, and a go.work stitching them together
// (which is what makes the go.sum trap reproducible).
func newForgeFixtureRepo(t *testing.T) string {
	t.Helper()
	for _, bin := range []string{"bash", "git", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("pkg/go.mod", "module github.com/reliant-labs/forge/pkg\n\ngo 1.24\n")
	write("pkg/svcerr/svcerr.go", "package svcerr\n\n// OK is a placeholder.\nconst OK = true\n")
	write("go.mod", "module github.com/reliant-labs/forge\n\ngo 1.24\n\nrequire github.com/reliant-labs/forge/pkg v0.1.0\n")
	write("cmd/forge/main.go", "package main\n\nfunc main() {}\n")
	write("internal/generator/project_pkgdep.go",
		"package generator\n\nconst defaultPublishedForgePkgVersion = \"v0.1.0\"\n")
	write("VERSION", "v0.1.0\n")
	write("internal/buildinfo/VERSION", "v0.1.0\n")
	// go.work is the point: it makes an in-workspace build succeed without
	// any forge/pkg hashes in go.sum, which is the trap the script closes.
	write("go.work", "go 1.24\n\nuse (\n\t.\n\tpkg\n)\n")

	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "config", "user.email", "test@example.com")
	gitIn(t, root, "config", "user.name", "test")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-q", "-m", "fixture")
	gitIn(t, root, "tag", "-a", "pkg/v0.1.0", "-m", "pkg v0.1.0")
	return root
}

func runForgeScript(t *testing.T, repo string, args ...string) (string, error) {
	t.Helper()
	script := releaseForgeScriptPath(t)
	full := append([]string{script, "--repo", repo}, args...)
	cmd := exec.CommandContext(context.Background(), "bash", full...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestReleaseForgeScript_DryRunPopulatesGoSum is the important one. It proves
// the script resolves the forge/pkg hashes for a version that has NOT been
// pushed anywhere — the circularity the old flow solved by pushing the pkg
// tag first, across two commits.
func TestReleaseForgeScript_DryRunPopulatesGoSum(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real go mod download against a local clone")
	}
	repo := newForgeFixtureRepo(t)
	out, err := runForgeScript(t, repo, "--dry-run", "v0.2.0")
	if err != nil {
		t.Fatalf("dry-run failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"go.sum: 2 entries for github.com/reliant-labs/forge/pkg v0.2.0",
		"DRY RUN: all validations passed",
		"would tag BOTH pkg/v0.2.0 and v0.2.0 at that one commit",
		"git push --atomic origin main pkg/v0.2.0 v0.2.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestReleaseForgeScript_DryRunLeavesNoTrace pins the restore behaviour. The
// script edits five files in place; a dry run that left any of them modified
// would both corrupt a shared checkout and make the NEXT run fail its own
// clean-tree gate.
func TestReleaseForgeScript_DryRunLeavesNoTrace(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real go mod download against a local clone")
	}
	repo := newForgeFixtureRepo(t)
	if out, err := runForgeScript(t, repo, "--dry-run", "v0.2.0"); err != nil {
		t.Fatalf("dry-run failed: %v\n%s", err, out)
	}
	if status := gitOut(t, repo, "status", "--porcelain"); status != "" {
		t.Errorf("dry run left the tree dirty:\n%s", status)
	}
	// go.sum did not exist before the run; restoring means removing it.
	if _, err := os.Stat(filepath.Join(repo, "go.sum")); !os.IsNotExist(err) {
		t.Errorf("dry run left a stray go.sum behind")
	}
	if tags := gitOut(t, repo, "tag", "-l", "v0.2.0", "pkg/v0.2.0"); tags != "" {
		t.Errorf("dry run created tags: %q", tags)
	}
}

// TestReleaseForgeScript_TagsBothAtOneCommit is the whole point of the
// command: the two tags Go forces on a multi-module repo must land on the
// SAME commit, which is what removes the push-ordering hazard.
func TestReleaseForgeScript_TagsBothAtOneCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real go mod download and a git push")
	}
	repo := newForgeFixtureRepo(t)
	// A bare remote so the script's atomic push has a destination.
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, repo, "clone", "--bare", "--quiet", repo, origin)
	gitIn(t, repo, "remote", "add", "origin", origin)

	out, err := runForgeScript(t, repo, "v0.2.0")
	if err != nil {
		t.Fatalf("release failed: %v\n%s", err, out)
	}

	head := gitOut(t, repo, "rev-parse", "HEAD")
	pkgTag := gitOut(t, repo, "rev-parse", "pkg/v0.2.0^{commit}")
	rootTag := gitOut(t, repo, "rev-parse", "v0.2.0^{commit}")
	if pkgTag != rootTag {
		t.Errorf("tags landed on different commits: pkg/v0.2.0=%s v0.2.0=%s", pkgTag, rootTag)
	}
	if pkgTag != head {
		t.Errorf("tags do not point at the release commit: tag=%s HEAD=%s", pkgTag, head)
	}

	// All three version files, plus go.mod/go.sum, in that ONE commit.
	files := gitOut(t, repo, "show", "--pretty=format:", "--name-only", "HEAD")
	for _, want := range []string{
		"VERSION",
		"internal/buildinfo/VERSION",
		"internal/generator/project_pkgdep.go",
		"go.mod",
		"go.sum",
	} {
		if !strings.Contains(files, want) {
			t.Errorf("release commit does not touch %s:\n%s", want, files)
		}
	}

	// The require and every version file must agree on the new version.
	gomod, err := os.ReadFile(filepath.Join(repo, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gomod), "github.com/reliant-labs/forge/pkg v0.2.0") {
		t.Errorf("go.mod does not require pkg v0.2.0:\n%s", gomod)
	}
	for _, vf := range []string{"VERSION", "internal/buildinfo/VERSION"} {
		got, err := os.ReadFile(filepath.Join(repo, vf))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != "v0.2.0" {
			t.Errorf("%s = %q, want v0.2.0", vf, strings.TrimSpace(string(got)))
		}
	}
	// The two VERSION files must stay byte-identical — buildinfo embeds a
	// copy and TestEmbeddedVersionFileMatchesSource enforces the match.
	rootV, _ := os.ReadFile(filepath.Join(repo, "VERSION"))
	embeddedV, _ := os.ReadFile(filepath.Join(repo, "internal/buildinfo/VERSION"))
	if string(rootV) != string(embeddedV) {
		t.Errorf("VERSION (%q) and internal/buildinfo/VERSION (%q) diverged", rootV, embeddedV)
	}
	pkgdep, err := os.ReadFile(filepath.Join(repo, "internal/generator/project_pkgdep.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pkgdep), `defaultPublishedForgePkgVersion = "v0.2.0"`) {
		t.Errorf("defaultPublishedForgePkgVersion not bumped:\n%s", pkgdep)
	}

	// The push is atomic, so the remote must have the branch AND both tags.
	remoteTags := gitOut(t, origin, "tag", "-l")
	for _, want := range []string{"pkg/v0.2.0", "v0.2.0"} {
		if !strings.Contains(remoteTags, want) {
			t.Errorf("tag %s did not reach the remote; got:\n%s", want, remoteTags)
		}
	}
}

func TestReleaseForgeScript_RejectsBadVersions(t *testing.T) {
	repo := newForgeFixtureRepo(t)
	for _, bad := range []string{"0.2.0", "pkg/v0.2.0", "v1.2", "latest", "v1.2.3+meta"} {
		out, err := runForgeScript(t, repo, "--dry-run", bad)
		if err == nil {
			t.Errorf("version %q: expected rejection, got success:\n%s", bad, out)
			continue
		}
		if !strings.Contains(out, "version must look like vX.Y.Z") {
			t.Errorf("version %q: unexpected error output:\n%s", bad, out)
		}
	}
}

func TestReleaseForgeScript_RejectsDirtyTree(t *testing.T) {
	repo := newForgeFixtureRepo(t)
	// Any dirty file blocks a release: the script commits root-module files,
	// so unrelated work would otherwise be swept into the release commit.
	if err := os.WriteFile(filepath.Join(repo, "unrelated.go"), []byte("package forge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runForgeScript(t, repo, "--dry-run", "v0.2.0")
	if err == nil {
		t.Fatalf("expected dirty-tree rejection, got success:\n%s", out)
	}
	if !strings.Contains(out, "working tree is not clean") {
		t.Errorf("unexpected error output:\n%s", out)
	}
}

// TestReleaseForgeScript_RejectsEitherExistingTag covers both tags
// independently: a half-finished earlier release leaves exactly one of them
// behind, and reusing it would publish immutable bytes under a used version.
func TestReleaseForgeScript_RejectsEitherExistingTag(t *testing.T) {
	for _, existing := range []string{"pkg/v0.2.0", "v0.2.0"} {
		t.Run(existing, func(t *testing.T) {
			repo := newForgeFixtureRepo(t)
			gitIn(t, repo, "tag", existing)
			out, err := runForgeScript(t, repo, "--dry-run", "v0.2.0")
			if err == nil {
				t.Fatalf("expected rejection for existing %s, got success:\n%s", existing, out)
			}
			if !strings.Contains(out, "already exists") {
				t.Errorf("unexpected error output:\n%s", out)
			}
		})
	}
}

func TestReleaseForgeScript_RejectsBrokenStandaloneBuild(t *testing.T) {
	repo := newForgeFixtureRepo(t)
	// Commit a compile error so the tree is clean but the standalone gate
	// fails — the consumer's view of the module, which go.work hides.
	if err := os.WriteFile(filepath.Join(repo, "pkg", "svcerr", "broken.go"),
		[]byte("package svcerr\n\nfunc broken() { undefinedSymbol() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "break build")

	out, err := runForgeScript(t, repo, "--dry-run", "v0.2.0")
	if err == nil {
		t.Fatalf("expected standalone-build rejection, got success:\n%s", out)
	}
	if !strings.Contains(out, "validating pkg module builds standalone") {
		t.Errorf("expected the build-validation banner before failure:\n%s", out)
	}
}

// TestReleaseForgeScript_RejectsWrongSubmodulePath guards the assumption
// behind the directory-prefixed tag: pkg/ must declare <root>/pkg, or
// pkg/vX.Y.Z is not the tag Go would look for.
func TestReleaseForgeScript_RejectsWrongSubmodulePath(t *testing.T) {
	repo := newForgeFixtureRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "pkg", "go.mod"),
		[]byte("module github.com/someone-else/pkg\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "wrong module path")

	out, err := runForgeScript(t, repo, "--dry-run", "v0.2.0")
	if err == nil {
		t.Fatalf("expected wrong-module rejection, got success:\n%s", out)
	}
	if !strings.Contains(out, "expected 'github.com/reliant-labs/forge/pkg'") {
		t.Errorf("unexpected error output:\n%s", out)
	}
}
