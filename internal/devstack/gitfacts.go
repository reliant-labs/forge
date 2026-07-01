// Package devstack owns forge's parallel-dev-stack primitives: the raw git
// facts that distinguish one working tree from another, and a memoized
// port-block allocator. Together they let N dev stacks (one per git
// worktree) run in parallel against shared clusters without colliding —
// DECLARATIVELY: forge supplies the facts + the allocator, KCL composes
// them.
//
// There is deliberately NO "instance" abstraction and NO user-visible
// index. forge exposes two raw git facts as KCL options and lets the KCL
// author decide which to key on:
//
//	option("worktree") -> the LINKED-worktree directory basename, or "" on
//	                      the PRIMARY checkout (any branch).
//	option("branch")   -> the current git branch, sanitized DNS-safe, always.
//
// and one resolved builtin that hides the port arithmetic entirely:
//
//	forge.allocate_port(base, key) -> base + block(key)*100
//
// where block(key) is a small int forge assigns the first time it sees key
// and persists (see blocks.go). The default — primary checkout, key "" —
// renders byte-identically to a stack with no dev-stack parameterization.
package devstack

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// maxNameLen bounds a sanitized git-fact value. These values feed k8s
// namespace suffixes, DB names, and NATS subject prefixes — all of which
// have length ceilings (a k8s namespace is 63 chars and already carries a
// project+env prefix), so we keep the fact segment short.
const maxNameLen = 24

var (
	nonDNS      = regexp.MustCompile(`[^a-z0-9-]+`)
	dashRuns    = regexp.MustCompile(`-+`)
	leadTrailDA = regexp.MustCompile(`^-+|-+$`)
)

// Sanitize lowercases s and reduces it to a DNS-safe label: [a-z0-9-],
// collapsed dash runs, no leading/trailing dash, bounded length. Returns
// "" when nothing survives (e.g. an all-symbol branch name).
func Sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonDNS.ReplaceAllString(s, "-")
	s = dashRuns.ReplaceAllString(s, "-")
	s = leadTrailDA.ReplaceAllString(s, "")
	if len(s) > maxNameLen {
		s = s[:maxNameLen]
		s = leadTrailDA.ReplaceAllString(s, "")
	}
	return s
}

// Worktree returns the LINKED-worktree directory basename for projectDir, or
// "" on the PRIMARY checkout (regardless of branch) and outside any git
// repo. This is the fact a consumer keys on when it wants the primary
// checkout to stay the DEFAULT stack on every branch — the everyday dev
// loop is then byte-identical to today, and only a `git worktree add`'ed
// checkout gets its own stack.
//
// The parallelism unit here is the WORKTREE, never the branch: branches
// change constantly and the primary checkout must always render default.
//
// Detection: a linked worktree's per-worktree git dir
// (`git rev-parse --absolute-git-dir` → …/.git/worktrees/<name>) differs
// from the repo's common dir (`--git-common-dir` → the primary's …/.git).
// The primary checkout has them equal. This is git's own authoritative
// distinction — far more robust than sniffing whether `.git` is a file vs
// a directory (which submodules and some tooling also make a file). The
// returned value is sanitized DNS-safe.
func Worktree(projectDir string) string {
	gitDir := gitOut(projectDir, "rev-parse", "--absolute-git-dir")
	commonDir := gitOut(projectDir, "rev-parse", "--git-common-dir")
	if gitDir == "" || commonDir == "" {
		return "" // not a git checkout
	}
	// Normalize commonDir to absolute so the comparison is exact:
	// --git-common-dir can come back relative (e.g. ".git"), and it is
	// relative to the command's working dir (projectDir), NOT the process
	// cwd — so resolve it against projectDir, not via filepath.Abs.
	if !filepath.IsAbs(commonDir) {
		base := projectDir
		if base == "" {
			if wd, err := os.Getwd(); err == nil {
				base = wd
			}
		}
		commonDir = filepath.Join(base, commonDir)
	}
	if sameDir(gitDir, commonDir) {
		return "" // primary checkout — DEFAULT, regardless of branch
	}
	// Linked worktree. Use the worktree's working-tree basename as the name.
	if root := gitOut(projectDir, "rev-parse", "--show-toplevel"); root != "" {
		return Sanitize(filepath.Base(root))
	}
	return ""
}

// Branch returns the current git branch for projectDir, sanitized DNS-safe,
// or "" outside a repo or on a detached HEAD. Unlike Worktree, Branch is
// reported for the primary checkout too — a consumer that WANTS to key on
// branch (e.g. a stack-per-branch workflow) can; one that wants the primary
// checkout to stay default keys on Worktree instead. The author chooses.
func Branch(projectDir string) string {
	b := gitOut(projectDir, "rev-parse", "--abbrev-ref", "HEAD")
	if b == "" || b == "HEAD" { // "HEAD" == detached
		return ""
	}
	return Sanitize(b)
}

// sameDir reports whether two paths point at the same directory, tolerant
// of symlinks (macOS /var vs /private/var) and trailing-slash differences.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	if ea == nil && eb == nil {
		return filepath.Clean(ra) == filepath.Clean(rb)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func gitOut(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
