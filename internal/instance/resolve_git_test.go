package instance

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// git runs a git command in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// initRepoOnBranch makes a primary git checkout in dir, with one commit, on
// a NAMED feature branch (not the default), and returns the branch name.
func initRepoOnBranch(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "test")
	git(t, dir, "checkout", "-q", "-b", "chore/bump-forge-93067142")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
}

// TestPrimaryCheckoutOnNamedBranchResolvesDefault is THE regression-lock for
// the a0bd658 bug: a primary checkout on a feature branch, with NO
// --instance, MUST resolve to the DEFAULT instance (so `forge up`/`deploy`
// renders the index-0 byte-identical ports). The branch name must NEVER
// leak in as an instance.
func TestPrimaryCheckoutOnNamedBranchResolvesDefault(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir()
	initRepoOnBranch(t, dir)

	inst, err := Resolve(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !inst.IsDefault() {
		t.Fatalf("primary checkout on a named branch must be DEFAULT, got %v "+
			"(the branch leaked in as an instance — the a0bd658 bug)", inst)
	}
	// And no registry entry was written for the branch.
	if list, _ := List(dir); len(list) != 0 {
		t.Errorf("primary checkout wrote a registry entry: %v", list)
	}
}

// TestExplicitInstanceWinsOnPrimaryCheckout: even on the primary checkout,
// an explicit --instance is honored (and indexed from 1).
func TestExplicitInstanceWinsOnPrimaryCheckout(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir()
	initRepoOnBranch(t, dir)

	inst, err := Resolve(dir, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Name != "alpha" || inst.Index != 1 {
		t.Fatalf("explicit --instance=alpha = %v, want alpha(#1)", inst)
	}
}

// TestLinkedWorktreeResolvesFromDirBasename: a `git worktree add`'ed
// checkout (NOT the primary) derives its instance from the worktree's
// directory basename — the actual multi-worktree workflow. The SAME repo's
// primary checkout stays default.
func TestLinkedWorktreeResolvesFromDirBasename(t *testing.T) {
	gitAvailable(t)
	primary := t.TempDir()
	initRepoOnBranch(t, primary)

	// Add a linked worktree at <tmp>/wt-feature on a new branch.
	wt := filepath.Join(t.TempDir(), "wt-feature")
	git(t, primary, "worktree", "add", "-q", "-b", "feature-x", wt)

	// The primary checkout is still DEFAULT.
	if inst, err := Resolve(primary, ""); err != nil || !inst.IsDefault() {
		t.Fatalf("primary checkout = %v (err %v), want DEFAULT", inst, err)
	}

	// The linked worktree resolves to its directory basename "wt-feature".
	inst, err := Resolve(wt, "")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Name != "wt-feature" {
		t.Fatalf("linked worktree instance = %q, want %q (from dir basename)", inst.Name, "wt-feature")
	}
	if inst.Index != 1 {
		t.Errorf("first linked-worktree index = %d, want 1", inst.Index)
	}
}

// TestNonRepoResolvesDefault: outside any git repo → default instance.
func TestNonRepoResolvesDefault(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir() // not git-init'ed
	inst, err := Resolve(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !inst.IsDefault() {
		t.Errorf("non-repo dir resolved %v, want DEFAULT", inst)
	}
}
