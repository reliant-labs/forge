package release

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitInit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("remote", "add", "origin", "git@github.com:acme/app.git")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-q", "-m", "init")
	return dir
}

func TestCaptureProvenance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real git repositories and worktrees; runs in task test")
	}
	dir := gitInit(t)
	ctx := context.Background()
	opts := CaptureOptions{ForgeVersion: "v0.1.44", WorktreeKey: "", Host: "h1"}

	clean, err := CaptureProvenance(ctx, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Dirty || clean.Branch != "main" || clean.Repo != "github.com/acme/app" || clean.ForgeVersion != "v0.1.44" || clean.Worktree.Label != "main" {
		t.Fatalf("clean capture: %+v", clean)
	}
	headTree, _ := gitOut(ctx, dir, "rev-parse", "HEAD^{tree}")
	if clean.Tree != headTree {
		t.Fatalf("a clean tree must equal HEAD^{tree}: %s vs %s", clean.Tree, headTree)
	}
	if err := clean.Validate(); err != nil {
		t.Fatal(err)
	}

	// Stage something, so we can prove the user's index is untouched.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "add", "a.txt").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("u\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stagedBefore, _ := gitOut(ctx, dir, "diff", "--cached", "--name-only")

	dirty, err := CaptureProvenance(ctx, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty.Dirty || dirty.Tree == "" || dirty.Tree == headTree {
		t.Fatalf("dirty capture must hash the built content: %+v", dirty)
	}
	again, _ := CaptureProvenance(ctx, dir, opts)
	if again.Tree != dirty.Tree {
		t.Fatal("two captures of one dirty state must agree")
	}
	stagedAfter, _ := gitOut(ctx, dir, "diff", "--cached", "--name-only")
	if stagedAfter != stagedBefore || strings.Contains(stagedAfter, "untracked.txt") {
		t.Fatalf("capture touched the user's index: before %q after %q", stagedBefore, stagedAfter)
	}

	// Not a git checkout: no commit, no error.
	plain, err := CaptureProvenance(ctx, t.TempDir(), opts)
	if err != nil || plain.Commit != "" || plain.Tree != "" {
		t.Fatalf("non-git dir: %+v %v", plain, err)
	}
}
