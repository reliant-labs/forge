package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitHeadSHA reads the HEAD commit of a repo dir. Test helper for the
// freshness tests, which need to compare a recorded build commit against
// the live HEAD.
func gitHeadSHA(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// gitignoreForgeState writes and commits a `.gitignore` that excludes
// `.forge/` in the repo, matching real forge projects. Without it, the
// `.forge/state/build-*.json` file WriteBuildState drops would show up as
// untracked and `git status --porcelain` would report a dirty tree —
// which would (correctly, in production-with-gitignore) never happen.
func gitignoreForgeState(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".forge/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e.x",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e.x",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("add", ".gitignore")
	run("commit", "-q", "-m", "gitignore forge state")
}

// gitCommitEmpty makes an additional commit in dir so HEAD advances past
// the previously-recorded build commit, simulating "you committed/pushed
// a fix after the last build."
func gitCommitEmpty(t *testing.T, dir, msg string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e.x",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e.x",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("commit", "-q", "--allow-empty", "-m", msg)
}

// TestResolveDeployImageTag_StaleCommitRefuses is the core fr-02d44d2b03
// guard: build state records commit C, HEAD has moved to C', the tree is
// clean — deploy must REFUSE rather than silently ship the old image.
func TestResolveDeployImageTag_StaleCommitRefuses(t *testing.T) {
	dir := newGitRepo(t)
	gitignoreForgeState(t, dir)
	builtCommit := gitHeadSHA(t, dir)
	// The build was at builtCommit; now advance HEAD past it.
	gitCommitEmpty(t, dir, "fix shipped after last build")

	if err := WriteBuildState(dir, "prod", BuildState{
		Tag:    "v0.1.0",
		Image:  "app",
		Commit: builtCommit,
		GitTag: "v0.1.0", // tagged, so the dirty/untagged warnings don't fire
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	_, _, _, err := resolveDeployImageTag(context.Background(), dir, "prod", "", false)
	if err == nil {
		t.Fatal("expected stale-image refusal, got nil error (would silently ship old code)")
	}
	if !strings.Contains(err.Error(), "stale") || !strings.Contains(err.Error(), "v0.1.0") {
		t.Fatalf("error should name the stale tag and explain; got: %v", err)
	}
}

// TestResolveDeployImageTag_FreshCommitAllows: build state commit ==
// HEAD, clean tree — the image is current, deploy proceeds.
func TestResolveDeployImageTag_FreshCommitAllows(t *testing.T) {
	dir := newGitRepo(t)
	gitignoreForgeState(t, dir)
	head := gitHeadSHA(t, dir)

	if err := WriteBuildState(dir, "prod", BuildState{
		Tag:    "v0.1.0",
		Image:  "app",
		Commit: head,
		GitTag: "v0.1.0",
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	tag, _, _, err := resolveDeployImageTag(context.Background(), dir, "prod", "", false)
	if err != nil {
		t.Fatalf("fresh build should deploy, got error: %v", err)
	}
	if tag != "v0.1.0" {
		t.Fatalf("tag = %q, want v0.1.0", tag)
	}
}

// TestResolveDeployImageTag_StaleButFlagOverrides: the --tag escape hatch
// bypasses build-state entirely, so the staleness guard never runs. This
// is the documented override for "I really do want to ship this tag."
func TestResolveDeployImageTag_StaleButFlagOverrides(t *testing.T) {
	dir := newGitRepo(t)
	gitignoreForgeState(t, dir)
	builtCommit := gitHeadSHA(t, dir)
	gitCommitEmpty(t, dir, "fix after build")
	if err := WriteBuildState(dir, "prod", BuildState{Tag: "v0.1.0", Commit: builtCommit}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	tag, _, src, err := resolveDeployImageTag(context.Background(), dir, "prod", "v0.1.0", false)
	if err != nil {
		t.Fatalf("--tag override should bypass the staleness guard, got: %v", err)
	}
	if tag != "v0.1.0" || !strings.Contains(src, "--tag") {
		t.Fatalf("override path: tag=%q src=%q", tag, src)
	}
}

// TestResolveDeployImageTag_DirtyTrackedFileSkipsFreshnessCheck: a tree
// with uncommitted edits to a TRACKED file has no single HEAD the build
// can be "behind," so the staleness guard is skipped (the dirty-build
// warning covers reproducibility). Even though the recorded commit
// differs from HEAD, deploy proceeds.
func TestResolveDeployImageTag_DirtyTrackedFileSkipsFreshnessCheck(t *testing.T) {
	dir := newGitRepo(t)
	gitignoreForgeState(t, dir)
	builtCommit := gitHeadSHA(t, dir)
	gitCommitEmpty(t, dir, "advance head")
	// Dirty a TRACKED file (README.md is committed by newGitRepo).
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteBuildState(dir, "prod", BuildState{Tag: "v0.1.0", Commit: builtCommit, Dirty: true}); err != nil {
		t.Fatalf("write state: %v", err)
	}

	tag, _, _, err := resolveDeployImageTag(context.Background(), dir, "prod", "", false)
	if err != nil {
		t.Fatalf("dirty tracked file should skip the staleness guard, got: %v", err)
	}
	if tag != "v0.1.0" {
		t.Fatalf("tag = %q, want v0.1.0", tag)
	}
}

// TestResolveDeployImageTag_UntrackedFileDoesNotMaskStaleness: an
// untracked file (editor dir, artifact) must NOT disable the guard — it
// doesn't move HEAD. A stale build with only untracked clutter present
// still refuses.
func TestResolveDeployImageTag_UntrackedFileDoesNotMaskStaleness(t *testing.T) {
	dir := newGitRepo(t)
	gitignoreForgeState(t, dir)
	builtCommit := gitHeadSHA(t, dir)
	gitCommitEmpty(t, dir, "advance head")
	// Add an UNTRACKED file — should be ignored by the clean check.
	if err := os.WriteFile(filepath.Join(dir, "scratch.tmp"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteBuildState(dir, "prod", BuildState{Tag: "v0.1.0", Commit: builtCommit, GitTag: "v0.1.0"}); err != nil {
		t.Fatalf("write state: %v", err)
	}
	_, _, _, err := resolveDeployImageTag(context.Background(), dir, "prod", "", false)
	if err == nil {
		t.Fatal("untracked clutter must not mask the staleness guard; expected refusal")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale refusal, got: %v", err)
	}
}

// TestResolveDeployImageTag_NoCommitSkipsFreshnessCheck: older build-state
// files predating commit-stamping have an empty Commit. Those must not be
// blocked — the guard only fires when it can prove staleness.
func TestResolveDeployImageTag_NoCommitSkipsFreshnessCheck(t *testing.T) {
	dir := newGitRepo(t)
	gitignoreForgeState(t, dir)
	gitCommitEmpty(t, dir, "advance head")
	if err := WriteBuildState(dir, "prod", BuildState{Tag: "legacy"}); err != nil {
		t.Fatalf("write state: %v", err)
	}
	tag, _, _, err := resolveDeployImageTag(context.Background(), dir, "prod", "", false)
	if err != nil {
		t.Fatalf("commit-less state should not be blocked, got: %v", err)
	}
	if tag != "legacy" {
		t.Fatalf("tag = %q, want legacy", tag)
	}
}
