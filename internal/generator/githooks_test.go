package generator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every hook forge writes must be recognisable as forge's: the CLI activates
// .githooks (core.hooksPath) only when IsForgeManagedGitHook accepts the
// pre-commit there. A header edit that broke the match would silently stop
// activating forge's own hooks in every fresh clone.
func TestGenerateGitHooks_AllRecognisedAsForgeManaged(t *testing.T) {
	g := &ProjectGenerator{Name: "demo", Path: t.TempDir()}
	if err := g.generateGitHooks(); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"pre-commit", "pre-push", "commit-msg"} {
		script, err := os.ReadFile(filepath.Join(g.Path, ".githooks", stage))
		if err != nil {
			t.Fatal(err)
		}
		if !IsForgeManagedGitHook(script) {
			t.Errorf(".githooks/%s is not recognised as forge-managed; its first lines:\n%.200s", stage, script)
		}
	}
}

// TestPrePushHook_LintsOnlyWhatThePushChanges runs the generated pre-push hook
// in a real repository, with a fake `forge` on PATH that records its
// arguments. The hook must lint the files the pushed commits touch that still
// exist — not the whole project, which measured 19 minutes on a 22-entity
// project — and must lint nothing when the push changes no file.
func TestPrePushHook_LintsOnlyWhatThePushChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("runs git and sh subprocesses; runs in task test")
	}
	repo := t.TempDir()
	bin := t.TempDir()
	argsLog := filepath.Join(t.TempDir(), "forge-args")
	if err := os.WriteFile(filepath.Join(bin, "forge"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >>"+argsLog+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("internal/old/old.go", "package old\n")
	write("internal/kept/kept.go", "package kept\n")
	git("add", "-A")
	git("commit", "-qm", "base")
	// What the remote already has: a remote-tracking ref at the base commit.
	git("update-ref", "refs/remotes/origin/main", "HEAD")

	write("internal/handlers/a/a.go", "package a\n")
	write("db/migrations/0002_x.up.sql", "SELECT 1;\n")
	git("rm", "-q", "internal/old/old.go")
	git("add", "-A")
	git("commit", "-qm", "change")
	head := git("rev-parse", "HEAD")

	runHook := func(stdin string) string {
		t.Helper()
		_ = os.Remove(argsLog)
		cmd := exec.Command("sh", "-c", prePushHook, "pre-push", "origin", "https://example.com/r.git")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FORGE_PRE_PUSH_FULL=")
		cmd.Stdin = strings.NewReader(stdin)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pre-push hook: %v\n%s", err, out)
		}
		got, _ := os.ReadFile(argsLog)
		return strings.TrimSpace(string(got))
	}

	zero := strings.Repeat("0", 40)
	got := runHook("refs/heads/main " + head + " refs/heads/main " + zero + "\n")
	want := "lint --quiet --scope db/migrations/0002_x.up.sql --scope internal/handlers/a/a.go"
	if got != want {
		t.Errorf("hook ran forge %q, want %q (only the pushed, still-existing files)", got, want)
	}

	// Deleting a remote branch pushes no commit, so there is nothing to lint.
	if got := runHook("(delete) " + zero + " refs/heads/old " + head + "\n"); got != "" {
		t.Errorf("a ref deletion ran forge %q, want nothing", got)
	}

	// Once the remote has the commits, a re-push changes nothing.
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	if got := runHook("refs/heads/main " + head + " refs/heads/main " + head + "\n"); got != "" {
		t.Errorf("a push of commits the remote already has ran forge %q, want nothing", got)
	}

	// A commit another remote already has is still new to the destination:
	// a contributor's branch fetched from upstream and pushed to origin.
	git("checkout", "-q", "-b", "topic", head)
	write("internal/up/up.go", "package up\n")
	git("add", "-A")
	git("commit", "-qm", "upstream work")
	git("update-ref", "refs/remotes/upstream/topic", "HEAD")
	topic := git("rev-parse", "HEAD")
	got = runHook("refs/heads/topic " + topic + " refs/heads/topic " + zero + "\n")
	if want := "lint --quiet --scope internal/up/up.go"; got != want {
		t.Errorf("a commit only upstream has ran forge %q, want %q", got, want)
	}

	// A merge whose parents the destination already has: its conflict
	// resolution is the only new code, and it must be linted.
	git("checkout", "-q", "-b", "side", head)
	write("internal/side/side.go", "package side\n")
	git("add", "-A")
	git("commit", "-qm", "side")
	git("update-ref", "refs/remotes/origin/side", "HEAD")
	git("checkout", "-q", "-b", "merged", head)
	git("merge", "-q", "--no-ff", "--no-commit", "side")
	write("internal/kept/kept.go", "package kept\n\n// resolved\n")
	git("add", "-A")
	git("commit", "-qm", "merge side")
	merged := git("rev-parse", "HEAD")
	got = runHook("refs/heads/merged " + merged + " refs/heads/merged " + zero + "\n")
	if want := "lint --quiet --scope internal/kept/kept.go --scope internal/side/side.go"; got != want {
		t.Errorf("a new merge ran forge %q, want %q (its resolution edit included)", got, want)
	}

	// An update to a ref the destination has, touching a name git quotes by
	// default (core.quotePath): the path reaches --scope as it is on disk.
	write("internal/café/café.go", "package cafe\n")
	git("add", "-A")
	git("commit", "-qm", "cafe")
	cafe := git("rev-parse", "HEAD")
	got = runHook("refs/heads/merged " + cafe + " refs/heads/merged " + merged + "\n")
	if want := "lint --quiet --scope internal/café/café.go"; got != want {
		t.Errorf("an update ran forge %q, want %q", got, want)
	}
}

// A project's own hook — what `project new --in-place` keeps — is never
// forge's, even when it mentions forge.
func TestIsForgeManagedGitHook_ProjectHooks(t *testing.T) {
	for name, script := range map[string]string{
		"empty":                  "",
		"no shebang":             "# forge-managed git pre-commit hook\nexit 0\n",
		"project hook":           "#!/bin/bash\n# Pre-commit hook to format staged Go files\nmake fmt\n",
		"mentions forge later":   "#!/bin/sh\n# our hook\n# forge-managed git pre-commit hook\nforge lint\n",
		"shebang only":           "#!/bin/sh",
		"header without shebang": "\n# forge-managed git pre-commit hook\n",
	} {
		if IsForgeManagedGitHook([]byte(script)) {
			t.Errorf("%s: IsForgeManagedGitHook(%q) = true, want false", name, script)
		}
	}
}
