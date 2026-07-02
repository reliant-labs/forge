package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ensureGitHooksActivated makes forge's committed git hooks fire in a
// fresh clone or new worktree without any explicit install step or new
// forge subcommand.
//
// The hook SCRIPT (.githooks/pre-commit, written by the generator)
// travels with the repo because it's tracked. The ACTIVATION —
// core.hooksPath — is LOCAL git config that `git clone` deliberately does
// not carry (git won't let a repo auto-run hooks on clone; that's the
// same reason tools like Husky exist). So we set it idempotently as a
// cheap side-effect of any forge command run inside the project: the
// first `forge generate` / `build` / `up` after a clone activates the
// hooks. Worktrees of an already-activated repo inherit it via the shared
// .git/config, so they need nothing.
//
// Deliberately conservative — it must never surprise the user or block a
// command:
//   - no-op unless we're in a forge project that ships .githooks/pre-commit
//     (skips pre-feature projects until their next `forge generate`),
//   - no-op unless it's a git repo,
//   - never clobbers a core.hooksPath the user pointed elsewhere; only
//     acts when it is unset,
//   - fully suppressible with FORGE_NO_HOOKS=1,
//   - best-effort: any git error is swallowed, never surfaced.
//
// root is the project root (the directory holding forge.yaml).
func ensureGitHooksActivated(root string) {
	if os.Getenv("FORGE_NO_HOOKS") != "" {
		return
	}
	// Only when the tracked hook is actually present. This gates the whole
	// feature on the generator having written .githooks/ — pre-feature
	// projects pick it up on their next `forge generate`.
	if _, err := os.Stat(filepath.Join(root, ".githooks", "pre-commit")); err != nil {
		return
	}
	// Must be a git repo. In a linked worktree .git is a file (a gitdir
	// pointer), not a directory, so Stat (not a dir check) is correct.
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return
	}
	// Respect an existing setting at any level — if the user (or a prior
	// run) already pointed core.hooksPath somewhere, leave it alone. Only
	// an unset value is ours to fill.
	cur, _ := exec.CommandContext(context.Background(), "git", "-C", root, "config", "--get", "core.hooksPath").Output()
	if strings.TrimSpace(string(cur)) != "" {
		return
	}
	// --local writes the repository config (the shared .git/config for
	// worktrees), so one write covers every linked worktree.
	if err := exec.CommandContext(context.Background(), "git", "-C", root, "config", "--local", "core.hooksPath", ".githooks").Run(); err != nil {
		return
	}
	fmt.Fprintln(os.Stderr,
		"forge: activated git hooks (.githooks) — commits now run 'forge lint' + 'forge audit'. Disable with FORGE_NO_HOOKS=1.")
}
