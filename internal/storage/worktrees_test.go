package storage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitRepoWithWorktree builds a real repository whose only commit is 60 days
// old, plus a linked worktree checked out at that same commit — merged into
// main by construction, so it passes every age and ancestry test Worktrees
// applies. Whatever the worktree holds is the only thing left to decide on.
func gitRepoWithWorktree(t *testing.T) (repo, worktree string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// Isolate from the developer's git config: a global hook or signing
	// setting must not decide whether this fixture can be built.
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	old := time.Now().Add(-60 * 24 * time.Hour).Format(time.RFC3339)
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	worktree = filepath.Join(root, "feature")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+old, "GIT_COMMITTER_DATE="+old)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("data/\n.env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", ".gitignore")
	git(repo, "commit", "-q", "-m", "init")
	git(repo, "worktree", "add", "-q", "-b", "feature", worktree)
	return repo, worktree
}

// ageDir backdates a directory so the worktree's own mtime is not what keeps it.
func ageDir(t *testing.T, dir string) {
	t.Helper()
	stamp := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(dir, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

// TestWorktreesRetainIgnoredAndUntrackedData is the B2 regression. `git
// status --porcelain` never lists IGNORED files, and `git worktree remove`
// without --force deletes them — so a worktree holding a gitignored database
// (reliant's ./data/, forge hostinfra's .forge/hostinfra/<name> postgres dir)
// or a gitignored .env looked clean and was deleted along with them.
//
// Real git throughout: what is being pinned is git's behaviour, which a faked
// `git status` would only restate.
func TestWorktreesRetainIgnoredAndUntrackedData(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
	}{
		{"ignored data directory", []string{"data/db.sqlite"}},
		{"ignored file", []string{".env"}},
		{"untracked file", []string{"notes.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, worktree := gitRepoWithWorktree(t)
			for _, rel := range tc.files {
				path := filepath.Join(worktree, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("irreplaceable"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ageDir(t, worktree)
			fakeLsof(t, unrelatedOpenFile, "exit 0")

			var out strings.Builder
			r := Runner{Out: &out}
			if err := r.Worktrees(context.Background(), repo, "main", 30*24*time.Hour, true); err != nil {
				t.Fatalf("Worktrees: %v\n%s", err, out.String())
			}
			for _, rel := range tc.files {
				if _, err := os.Stat(filepath.Join(worktree, rel)); err != nil {
					t.Fatalf("worktrees --apply deleted %s (%v)\n%s", rel, err, out.String())
				}
			}
		})
	}
}

// TestWorktreesRemoveATrulyCleanWorktree is the positive control for the test
// above: without it, a Worktrees that never removed anything would pass.
func TestWorktreesRemoveATrulyCleanWorktree(t *testing.T) {
	repo, worktree := gitRepoWithWorktree(t)
	ageDir(t, worktree)
	fakeLsof(t, unrelatedOpenFile, "exit 0")
	var out strings.Builder
	if err := (Runner{Out: &out}).Worktrees(context.Background(), repo, "main", 30*24*time.Hour, true); err != nil {
		t.Fatalf("Worktrees: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("a clean, merged, idle worktree was not removed (%v)\n%s", err, out.String())
	}
}

// TestWorktreesRetainAWorktreeAProcessIsUsing: the only in-use test used to
// be "is it forge's OWN cwd". A dev stack, a shell or an editor sitting in
// the worktree passed that and lost its directory. lsof reports the RESOLVED
// path (macOS: /private/var/... for /var/...), so this also pins that the
// comparison is made on canonical paths.
func TestWorktreesRetainAWorktreeAProcessIsUsing(t *testing.T) {
	for _, tc := range []struct{ name, record string }{
		{"process cwd inside the worktree", "fcwd\nn%s/sub\n"},
		{"process cwd is the worktree", "fcwd\nn%s\n"},
		{"file open inside the worktree", "f7\nn%s/README\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, worktree := gitRepoWithWorktree(t)
			ageDir(t, worktree)
			fakeLsof(t, "p4242\n"+strings.ReplaceAll(tc.record, "%s", resolved(t, worktree)), "exit 0")
			var out strings.Builder
			if err := (Runner{Out: &out}).Worktrees(context.Background(), repo, "main", 30*24*time.Hour, true); err != nil {
				t.Fatalf("Worktrees: %v\n%s", err, out.String())
			}
			if _, err := os.Stat(worktree); err != nil {
				t.Fatalf("removed a worktree a live process is using (%v)\n%s", err, out.String())
			}
		})
	}
}

// TestWorktreesRefuseWhenUseCannotBeDetermined: with no evidence about which
// worktrees are in use, removal must not proceed on age alone.
func TestWorktreesRefuseWhenUseCannotBeDetermined(t *testing.T) {
	repo, worktree := gitRepoWithWorktree(t)
	ageDir(t, worktree)
	fakeLsof(t, "", "exit 2")
	var out strings.Builder
	err := (Runner{Out: &out}).Worktrees(context.Background(), repo, "main", 30*24*time.Hour, true)
	if _, statErr := os.Stat(worktree); statErr != nil {
		t.Fatalf("removed a worktree with no in-use evidence (%v)\n%s", statErr, out.String())
	}
	if err == nil {
		t.Fatalf("an unknowable in-use set must be reported, not silently passed:\n%s", out.String())
	}
}
