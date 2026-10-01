// File: internal/migrationgit/migrationgit_test.go
//
// This package is the single definition of "already on the default branch",
// which the version lint and `forge db migration rebase` both act on in
// opposite directions — the lint flags what is new, rebase refuses what is
// merged. The tests here pin the two behaviours that keep that honest: the
// merge-base answer itself, and the "cannot know" contract, which is what
// every caller branches on when git is unavailable.

package migrationgit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repo(t *testing.T, onMain, onBranch []string) (repoRoot, migDir string) {
	t.Helper()

	repoRoot = t.TempDir()
	migDir = filepath.Join(repoRoot, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(migDir, name), []byte("SELECT 1;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	for _, n := range onMain {
		write(n)
	}
	git("add", ".")
	git("commit", "-q", "-m", "main")
	git("checkout", "-q", "-b", "feature")
	for _, n := range onBranch {
		write(n)
	}
	if len(onBranch) > 0 {
		git("add", ".")
		git("commit", "-q", "-m", "feature")
	}
	return repoRoot, migDir
}

// The mark is the state the branch was cut FROM. A migration added on the
// branch must not raise it, or the lint would consider the branch's own files
// pre-existing and never flag them.
func TestMergeBaseMaxIsTheStateTheBranchWasCutFrom(t *testing.T) {
	repoRoot, migDir := repo(t,
		[]string{"20260101120000_add_users.up.sql"},
		[]string{"20260501000000_add_sessions.up.sql"},
	)

	max, ok := MergeBaseMax(RepoRoot(repoRoot), migDir)
	if !ok {
		t.Fatal("merge-base unknown in a repository that has a main branch")
	}
	if max != 20260101120000 {
		t.Errorf("MergeBaseMax = %d, want 20260101120000 — the branch's own migration must not raise the mark", max)
	}
}

// The default branch's TIP, not the merge-base: a migration that landed on
// main after this branch was cut is just as merged, and renaming it is just
// as wrong.
func TestDefaultBranchVersionsSeesCommitsMadeAfterTheBranchWasCut(t *testing.T) {
	repoRoot, migDir := repo(t,
		[]string{"20260101120000_add_users.up.sql"},
		[]string{"20260501000000_add_sessions.up.sql"},
	)

	// Land another migration on main after the feature branch exists.
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(migDir, "20260401000000_add_teams.up.sql"), []byte("SELECT 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "later on main")
	git("checkout", "-q", "feature")

	versions, ok := DefaultBranchVersions(RepoRoot(repoRoot), migDir)
	if !ok {
		t.Fatal("default branch versions unknown")
	}
	if _, found := versions[20260401000000]; !found {
		t.Errorf("version landed on main after the branch was cut is not reported as merged; got %v", versions)
	}
	if _, found := versions[20260501000000]; found {
		t.Error("the feature branch's own migration must not be reported as merged")
	}
}

// THE "CANNOT KNOW" CONTRACT. Outside a repository every lookup reports
// false, and callers depend on that specific signal: the lint falls back to a
// directory-local rule and rebase refuses --all-pending rather than offering
// to rename merged history. An error or a zero-that-looks-real here would
// turn both into silent wrong answers.
func TestEveryLookupReportsUnknownOutsideAGitRepository(t *testing.T) {
	dir := t.TempDir()

	if _, ok := MergeBaseMax(RepoRoot(dir), dir); ok {
		t.Error("MergeBaseMax claims to know the merge-base outside a git repository")
	}
	if _, ok := DefaultBranchVersions(RepoRoot(dir), dir); ok {
		t.Error("DefaultBranchVersions claims to know the default branch outside a git repository")
	}
	if IsTracked(RepoRoot(dir), filepath.Join(dir, "whatever.up.sql")) {
		t.Error("IsTracked reports a file in no repository as tracked")
	}
}

// A tracked rename goes through `git mv` so it is staged as a rename rather
// than a delete plus an untracked add — which is what makes the version
// change reviewable as one file.
func TestMoveStagesATrackedRename(t *testing.T) {
	repoRoot, migDir := repo(t, []string{"20260101120000_add_users.up.sql"}, nil)

	from := filepath.Join(migDir, "20260101120000_add_users.up.sql")
	to := filepath.Join(migDir, "20260601000000_add_users.up.sql")
	if err := Move(RepoRoot(repoRoot), from, to); err != nil {
		t.Fatalf("Move: %v", err)
	}

	if _, err := os.Stat(to); err != nil {
		t.Fatalf("destination missing after Move: %v", err)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Error("source still exists after Move")
	}

	status := exec.Command("git", "status", "--porcelain")
	status.Dir = repoRoot
	out, err := status.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "R ") {
		t.Errorf("git does not see a staged rename, so the diff will read as an unrelated new file; status:\n%s", out)
	}
}

// An untracked file still moves. A migration created but not yet added is the
// common case for a rebase — the author noticed before committing.
func TestMoveRenamesAnUntrackedFile(t *testing.T) {
	repoRoot, migDir := repo(t, []string{"20260101120000_add_users.up.sql"}, nil)

	from := filepath.Join(migDir, "20260201000000_untracked.up.sql")
	if err := os.WriteFile(from, []byte("SELECT 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(migDir, "20260601000000_untracked.up.sql")
	if err := Move(RepoRoot(repoRoot), from, to); err != nil {
		t.Fatalf("Move on an untracked file: %v", err)
	}
	if _, err := os.Stat(to); err != nil {
		t.Errorf("untracked file was not moved: %v", err)
	}
}
