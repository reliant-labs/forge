package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/config"
)

// These tests pin what `forge project new --in-place` may do to a directory
// the user already owns. Reproduced on a real product repo (Bark Social):
// the in-place scaffold ran `git init` + `git add .` + `git commit` on top of
// the user's history, sweeping their untracked files into a forge-authored
// commit, and silently replaced their README.md and .gitignore — the lost
// ignore entries being exactly why the untracked directories got swept in.
//
// They drive the REAL command tree (NewRootCmd, `-C <target>`), not runNew
// directly: the root PersistentPreRunE is where forge self-activates git
// hooks, and a direct runNew call would skip it. --kind library walks the
// same in-place write + git finalize code quickly; the service case adds the
// proto bootstrap, whose buf run re-executes forge as protoc-gen-forge — the
// other route by which a scaffold used to write core.hooksPath into the
// user's repository.

// isolateGit gives git a hermetic identity and no user/system config, so
// the fresh-directory case can commit on any machine (and CI) and no
// global commit.gpgsign or hook setting leaks in.
func isolateGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	emptyCfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(emptyCfg, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", emptyCfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "forge-test")
	t.Setenv("GIT_AUTHOR_EMAIL", "forge-test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "forge-test")
	t.Setenv("GIT_COMMITTER_EMAIL", "forge-test@example.com")
	// Hook activation must be ON (empty == unset for ensureGitHooksActivated):
	// the existing-repo case asserts forge declines to configure hooks by
	// itself, not because the user opted out.
	t.Setenv("FORGE_NO_HOOKS", "")
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedUserRepo makes dir an existing product repo: one commit carrying a
// custom README.md and .gitignore, plus an untracked notes file.
func seedUserRepo(t *testing.T, dir string) (head string) {
	t.Helper()
	gitOut(t, dir, "init", "-q")
	writeTestFile(t, filepath.Join(dir, "README.md"), userReadme)
	writeTestFile(t, filepath.Join(dir, ".gitignore"), userGitignore)
	gitOut(t, dir, "add", "README.md", ".gitignore")
	gitOut(t, dir, "commit", "-q", "-m", "user history")
	return gitOut(t, dir, "rev-parse", "HEAD")
}

const (
	userReadme    = "# Bark Social\n\nThe product README. Forge must not replace this.\n"
	userGitignore = "# my ignores\n.agent-artifacts/\n.env.local\n"
)

// runInPlace runs `forge -C <dir> project new --in-place` through the real
// root command, so the root pre-run hooks fire exactly as they do for a user.
func runInPlace(t *testing.T, dir, kind string, force bool) {
	t.Helper()
	t.Cleanup(func() { _ = cmdutil.SetProjectDir("") })
	args := []string{"-C", dir, "project", "new", "--in-place", "--name", "hounders",
		"--mod", "github.com/example/hounders", "--path", dir, "--kind", kind, "--skip-tools"}
	if kind == config.ProjectKindService {
		// The test binary is an unreleased build: a service scaffold from it
		// must be bridged to this checkout, and that is opt-in.
		args = append(args, "--service", "membership", "--link-forge")
	}
	if force {
		args = append(args, "--force")
	}
	root := NewRootCmd()
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("forge %s: %v", strings.Join(args, " "), err)
	}
}

func TestRunNewInPlace_ExistingRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("scaffolds projects into real git repositories; runs in task test")
	}
	isolateGit(t)

	for _, tc := range []struct {
		name string
		// sub is the scaffold target relative to the repo root; "" = the root.
		sub  string
		kind string
	}{
		{name: "repo root", sub: "", kind: config.ProjectKindLibrary},
		{name: "monorepo subdirectory", sub: "services/hounders", kind: config.ProjectKindLibrary},
		{name: "service with proto bootstrap", sub: "", kind: config.ProjectKindService},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.kind == config.ProjectKindService {
				if testing.Short() {
					t.Skip("runs the real proto bootstrap (buf + protoc plugins)")
				}
				requireProtoToolchain(t)
			}
			repo := t.TempDir()
			head := seedUserRepo(t, repo)
			target := filepath.Join(repo, tc.sub)
			writeTestFile(t, filepath.Join(repo, "notes.txt"), "untracked user notes\n")
			if tc.sub != "" {
				// The subdirectory owns its own README + .gitignore.
				writeTestFile(t, filepath.Join(target, "README.md"), userReadme)
				writeTestFile(t, filepath.Join(target, ".gitignore"), userGitignore)
			}

			runInPlace(t, target, tc.kind, false)

			if got := gitOut(t, repo, "rev-parse", "HEAD"); got != head {
				t.Errorf("HEAD moved %s -> %s: forge committed into the user's repository", head, got)
			}
			if staged := gitOut(t, repo, "diff", "--cached", "--name-only"); staged != "" {
				t.Errorf("forge staged files in the user's index:\n%s", staged)
			}
			if st := gitOut(t, repo, "status", "--porcelain", "--", "notes.txt"); st != "?? notes.txt" {
				t.Errorf("untracked notes.txt status = %q, want it still untracked", st)
			}
			if tc.sub != "" {
				if _, err := os.Stat(filepath.Join(target, ".git")); !os.IsNotExist(err) {
					t.Errorf("a nested .git was created in %s (stat err = %v)", target, err)
				}
			}
			assertHooksPathUnset(t, repo, "after the scaffold")

			readme, _ := os.ReadFile(filepath.Join(target, "README.md"))
			if string(readme) != userReadme {
				t.Errorf("README.md was overwritten:\n%s", readme)
			}
			ignore1, _ := os.ReadFile(filepath.Join(target, ".gitignore"))
			if !strings.HasPrefix(string(ignore1), userGitignore) {
				t.Errorf(".gitignore lost the user's entries:\n%s", ignore1)
			}
			for _, want := range []string{forgeGitignoreBegin, "go.work.sum", forgeGitignoreEnd} {
				if !strings.Contains(string(ignore1), want) {
					t.Errorf(".gitignore is missing forge's %q:\n%s", want, ignore1)
				}
			}

			// Re-running (with --force, the only way past an existing
			// forge.yaml) must not grow the forge block.
			runInPlace(t, target, tc.kind, true)
			assertHooksPathUnset(t, repo, "after a --force re-run")
			ignore2, _ := os.ReadFile(filepath.Join(target, ".gitignore"))
			if string(ignore2) != string(ignore1) {
				t.Errorf(".gitignore merge is not idempotent.\nfirst:\n%s\nsecond:\n%s", ignore1, ignore2)
			}
			if got := gitOut(t, repo, "rev-parse", "HEAD"); got != head {
				t.Errorf("HEAD moved on re-run: %s -> %s", head, got)
			}
		})
	}
}

// assertHooksPathUnset fails when forge wrote core.hooksPath into the user's
// repository config. That config is theirs: `project new` in an existing
// repo must leave it alone, and the offer to activate hooks is made by a
// later forge command (ensureGitHooksActivated), which honours FORGE_NO_HOOKS.
func assertHooksPathUnset(t *testing.T, repo, when string) {
	t.Helper()
	if hp := gitOut(t, repo, "config", "--local", "--default", "", "core.hooksPath"); hp != "" {
		t.Errorf("core.hooksPath = %q %s: forge reconfigured the user's repository", hp, when)
	}
}

func TestRunNewInPlace_FreshDirectoryStillInitsGit(t *testing.T) {
	isolateGit(t)
	dir := t.TempDir()

	runInPlace(t, dir, config.ProjectKindLibrary, false)

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("fresh directory was not git-initialised: %v", err)
	}
	if msg := gitOut(t, dir, "log", "-1", "--format=%s"); msg != "Initial commit from forge" {
		t.Errorf("last commit = %q, want the forge initial commit", msg)
	}
	if hp := gitOut(t, dir, "config", "--local", "core.hooksPath"); hp != ".githooks" {
		t.Errorf("core.hooksPath = %q, want .githooks", hp)
	}
}
