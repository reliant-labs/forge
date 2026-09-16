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

	// ONE module, with pkg/ as a plain directory inside it — the shape a
	// release now has to handle. No pkg/go.mod, no go.work, and no
	// self-require, which is why the script no longer needs to resolve an
	// unpushed version's hashes into go.sum.
	write("go.mod", "module github.com/reliant-labs/forge\n\ngo 1.24\n")
	write("pkg/svcerr/svcerr.go", "package svcerr\n\n// OK is a placeholder.\nconst OK = true\n")
	write("cmd/forge/main.go", "package main\n\nfunc main() {}\n")
	write("VERSION", "v0.1.0\n")
	write("internal/buildinfo/VERSION", "v0.1.0\n")

	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "config", "user.email", "test@example.com")
	gitIn(t, root, "config", "user.name", "test")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-q", "-m", "fixture")
	gitIn(t, root, "tag", "-a", "v0.1.0", "-m", "forge v0.1.0")
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

// TestReleaseForgeScript_DryRunLeavesNoTrace: a dry run validates and edits,
// then restores every file it touched — the checkout is shared with other
// agents, so a dry run must be invisible.
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
// TestReleaseForgeScript_TagsOnceAtTheReleaseCommit: one module, one tag, on
// the commit that syncs the version files. This replaced a test asserting
// that pkg/vX.Y.Z and vX.Y.Z landed on the SAME commit — an invariant that
// only had to be asserted because there were two tags to get wrong.
func TestReleaseForgeScript_TagsOnceAtTheReleaseCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a git push")
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
	tag := gitOut(t, repo, "rev-parse", "v0.2.0^{commit}")
	if tag != head {
		t.Errorf("tag does not point at the release commit: tag=%s HEAD=%s", tag, head)
	}

	// Only the version files move now: there is no require to bump and no
	// go.sum to populate.
	files := gitOut(t, repo, "show", "--pretty=format:", "--name-only", "HEAD")
	for _, want := range []string{"VERSION", "internal/buildinfo/VERSION"} {
		if !strings.Contains(files, want) {
			t.Errorf("release commit does not touch %s:\n%s", want, files)
		}
	}
	if strings.Contains(files, "go.mod") {
		t.Errorf("release commit touches go.mod — nothing in a single-module release should:\n%s", files)
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
	// The push is atomic, so the remote must have the branch AND the tag.
	if remoteTags := gitOut(t, origin, "tag", "-l"); !strings.Contains(remoteTags, "v0.2.0") {
		t.Errorf("tag v0.2.0 did not reach the remote; got:\n%s", remoteTags)
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
// TestReleaseForgeScript_RejectsExistingTag: versions are immutable, so a
// tag that already exists locally is a hard stop.
func TestReleaseForgeScript_RejectsExistingTag(t *testing.T) {
	repo := newForgeFixtureRepo(t)
	gitIn(t, repo, "tag", "v0.2.0")
	out, err := runForgeScript(t, repo, "--dry-run", "v0.2.0")
	if err == nil {
		t.Fatalf("expected rejection for an existing tag, got success:\n%s", out)
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("unexpected error output:\n%s", out)
	}
}
