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
	// Activity includes the worktree's git admin files, fresh from `worktree add`.
	if admin := worktreeAdminDir(dir); admin != "" {
		for _, name := range []string{"index", "HEAD", "logs/HEAD"} {
			_ = os.Chtimes(filepath.Join(admin, name), stamp, stamp)
		}
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
			if err := r.Worktrees(context.Background(), repo, "main", 24*time.Hour, true); err != nil {
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
	if err := (Runner{Out: &out}).Worktrees(context.Background(), repo, "main", 24*time.Hour, true); err != nil {
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
			if err := (Runner{Out: &out}).Worktrees(context.Background(), repo, "main", 24*time.Hour, true); err != nil {
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
	err := (Runner{Out: &out}).Worktrees(context.Background(), repo, "main", 24*time.Hour, true)
	if _, statErr := os.Stat(worktree); statErr != nil {
		t.Fatalf("removed a worktree with no in-use evidence (%v)\n%s", statErr, out.String())
	}
	if err == nil {
		t.Fatalf("an unknowable in-use set must be reported, not silently passed:\n%s", out.String())
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func reapOnce(t *testing.T, r Runner, repo string, apply bool) WorktreeReport {
	t.Helper()
	report, err := r.reapWorktrees(context.Background(), reapOptions{repos: []string{repo}, base: "main", idle: 24 * time.Hour, apply: apply})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func heldReason(report WorktreeReport, path string) string {
	path, _ = filepath.EvalSymlinks(path)
	for _, h := range report.Held {
		if h.Path == path {
			return h.Reason
		}
	}
	return ""
}

func TestWorktreeAllowsRebuildableIgnoredAndHoldsTheRest(t *testing.T) {
	for _, tc := range []struct {
		name, file, want string
	}{
		{"node_modules", "web/node_modules/x/index.js", ""},
		{"build output", "dist/app.js", ""},
		{"app data", "data/db.sqlite", HoldData},
		{"env file", ".env", HoldData},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, worktree := gitRepoWithWorktree(t)
			ignore := filepath.Join(repo, ".git", "info", "exclude")
			if err := os.WriteFile(ignore, []byte("node_modules/\ndist/\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(worktree, tc.file)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			ageDir(t, worktree)
			fakeLsof(t, unrelatedOpenFile, "exit 0")
			report := reapOnce(t, Runner{}, repo, false)
			if got := heldReason(report, worktree); got != tc.want {
				t.Fatalf("held reason = %q, want %q (removable %v, held %+v)", got, tc.want, report.Removable, report.Held)
			}
		})
	}
}

func TestWorktreeHoldsLockedRecentAndUnpushed(t *testing.T) {
	t.Run("locked", func(t *testing.T) {
		repo, worktree := gitRepoWithWorktree(t)
		runGit(t, repo, "worktree", "lock", worktree)
		ageDir(t, worktree)
		fakeLsof(t, unrelatedOpenFile, "exit 0")
		if got := heldReason(reapOnce(t, Runner{}, repo, true), worktree); got != HoldLocked {
			t.Fatalf("got %q", got)
		}
		if _, err := os.Stat(worktree); err != nil {
			t.Fatal("locked worktree removed")
		}
	})
	t.Run("recently active", func(t *testing.T) {
		repo, worktree := gitRepoWithWorktree(t)
		ageDir(t, worktree)
		// A fresh index (git add, checkout) is activity even if the dir mtime is old.
		now := time.Now()
		if err := os.Chtimes(filepath.Join(worktreeAdminDir(worktree), "index"), now, now); err != nil {
			t.Fatal(err)
		}
		fakeLsof(t, unrelatedOpenFile, "exit 0")
		if got := heldReason(reapOnce(t, Runner{}, repo, false), worktree); got != HoldActive {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("unpushed", func(t *testing.T) {
		repo, worktree := gitRepoWithWorktree(t)
		if err := os.WriteFile(filepath.Join(worktree, "new.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, worktree, "add", "new.txt")
		runGit(t, worktree, "commit", "-q", "-m", "work")
		ageDir(t, worktree)
		fakeLsof(t, unrelatedOpenFile, "exit 0")
		if got := heldReason(reapOnce(t, Runner{}, repo, true), worktree); got != HoldUnpushed {
			t.Fatalf("got %q", got)
		}
		if _, err := os.Stat(worktree); err != nil {
			t.Fatal("unpushed worktree removed")
		}
	})
}

func TestWorktreePushedToRemoteBranchIsRemovable(t *testing.T) {
	repo, worktree := gitRepoWithWorktree(t)
	if err := os.WriteFile(filepath.Join(worktree, "new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, worktree, "add", "new.txt")
	runGit(t, worktree, "commit", "-q", "-m", "work")
	head := strings.TrimSpace(runGit(t, worktree, "rev-parse", "HEAD"))
	// A remote-tracking ref containing HEAD, as `git push` would leave.
	runGit(t, repo, "update-ref", "refs/remotes/origin/feature", head)
	ageDir(t, worktree)
	fakeLsof(t, unrelatedOpenFile, "exit 0")
	report := reapOnce(t, Runner{}, repo, true)
	if len(report.Removed) != 1 {
		t.Fatalf("pushed idle worktree not removed: held %v", report.Held)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatal("worktree still exists")
	}
	if !strings.Contains(runGit(t, repo, "branch", "--list", "feature"), "feature") {
		t.Fatal("branch was deleted")
	}
}

func TestWorktreeLayerPreviewsUnlessPolicyOptsIn(t *testing.T) {
	for _, reap := range []bool{false, true} {
		repo, worktree := gitRepoWithWorktree(t)
		runGit(t, repo, "update-ref", "refs/remotes/origin/main", "main")
		ageDir(t, worktree)
		fakeLsof(t, unrelatedOpenFile, "exit 0")
		r := Runner{Policy: Policy{Repos: []string{repo}, WorktreeReap: reap}}
		r.Ctx = context.Background()
		if err := r.worktreeLayer(true); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(worktree)
		if gone := os.IsNotExist(err); gone != reap {
			t.Fatalf("worktree_reap=%v but removed=%v", reap, gone)
		}
	}
}

func TestRebuildablePathNeverMatchesData(t *testing.T) {
	allow := append(DefaultWorktreeRebuildable(), "data", ".env")
	for _, p := range []string{"data/", ".env", ".env.local", ".forge/hostinfra/pg/", "x/secrets/", "a.db"} {
		if rebuildablePath(p, allow) {
			t.Errorf("%s must never be rebuildable", p)
		}
	}
	for _, p := range []string{"node_modules/", "web/node_modules/", "frontends/a/tsconfig.tsbuildinfo", ".forge/logs/"} {
		if !rebuildablePath(p, DefaultWorktreeRebuildable()) {
			t.Errorf("%s should be rebuildable", p)
		}
	}
}

func TestConvergeRecordsRepos(t *testing.T) {
	repo := t.TempDir()
	if _, changed := upsertFacts(Policy{}, Facts{Repos: []string{repo}}); changed {
		t.Fatal("a repo under the temp dir must not be recorded")
	}
	p, changed := upsertFacts(Policy{}, Facts{Repos: []string{"/Users/x/src/reliant", "/Users/x/src/reliant"}})
	if !changed || len(p.Repos) != 1 {
		t.Fatalf("repos = %v", p.Repos)
	}
}

// The scan must not reset the idle clock it measures: a stale stat cache makes
// plain `git status` rewrite the index.
func TestWorktreeScanDoesNotTouchIndex(t *testing.T) {
	repo, worktree := gitRepoWithWorktree(t)
	readme := filepath.Join(worktree, ".gitignore")
	content, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(readme, content, 0o600); err != nil { // same content, new mtime
		t.Fatal(err)
	}
	ageDir(t, worktree)
	index := filepath.Join(worktreeAdminDir(worktree), "index")
	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	fakeLsof(t, unrelatedOpenFile, "exit 0")
	reapOnce(t, Runner{}, repo, false)
	after, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("scan rewrote the worktree index: %v -> %v", before.ModTime(), after.ModTime())
	}
}

func TestWorktreeHoldsHiddenChanges(t *testing.T) {
	for _, flag := range []string{"--skip-worktree", "--assume-unchanged"} {
		t.Run(flag, func(t *testing.T) {
			repo, worktree := gitRepoWithWorktree(t)
			runGit(t, worktree, "update-index", flag, ".gitignore")
			if err := os.WriteFile(filepath.Join(worktree, ".gitignore"), []byte("data/\n.env\nmy secret edit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			ageDir(t, worktree)
			fakeLsof(t, unrelatedOpenFile, "exit 0")
			report := reapOnce(t, Runner{}, repo, true)
			if got := heldReason(report, worktree); got != HoldHidden {
				t.Fatalf("held reason = %q, want %q (held %+v)", got, HoldHidden, report.Held)
			}
			if _, err := os.Stat(worktree); err != nil {
				t.Fatal("worktree with hidden edits was removed")
			}
		})
	}
}

func TestWorktreeHoldsNestedRepositories(t *testing.T) {
	for _, tc := range []struct{ name, dir string }{
		{"inside allowlisted node_modules", "node_modules/pkg/.git"},
		{"inside allowlisted bin, deep", "bin/a/b/c/.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, worktree := gitRepoWithWorktree(t)
			if err := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte("node_modules/\nbin/\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(worktree, tc.dir), 0o700); err != nil {
				t.Fatal(err)
			}
			ageDir(t, worktree)
			fakeLsof(t, unrelatedOpenFile, "exit 0")
			report := reapOnce(t, Runner{}, repo, true)
			if got := heldReason(report, worktree); got != HoldNested {
				t.Fatalf("held reason = %q, want %q (held %+v)", got, HoldNested, report.Held)
			}
			if _, err := os.Stat(worktree); err != nil {
				t.Fatal("worktree holding a nested repository was removed")
			}
		})
	}
}

// A process that starts using the worktree AFTER the batch snapshot but before
// removal must stop the removal. The fake lsof reports nothing on its first
// run and the worktree on every later run.
func TestWorktreeRecheckCatchesUseAfterBatchClassification(t *testing.T) {
	repo, worktree := gitRepoWithWorktree(t)
	ageDir(t, worktree)
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	script := "#!/bin/sh\nif [ -e '" + counter + "' ]; then printf 'p4242\\nfcwd\\nn" + resolved(t, worktree) + "\\n'; else : > '" + counter + "'; printf 'p1\\nf3\\nn/nonexistent/x\\n'; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "lsof"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	report := reapOnce(t, Runner{}, repo, true)
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree removed although a process started using it after the batch snapshot (held %+v)", report.Held)
	}
	if got := heldReason(report, worktree); got != HoldInUse {
		t.Fatalf("held reason = %q, want %q", got, HoldInUse)
	}
	if len(report.Removed) != 0 {
		t.Fatalf("removed %v", report.Removed)
	}
}
