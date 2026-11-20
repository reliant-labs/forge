package cigate

import (
	"context"
	"strings"
)

// BranchRange is the commits a checkout adds over the branch it merges into,
// as a `git log` revision range, with a sentence saying how it was chosen:
//
//   - on a branch: `<merge-base>..HEAD`, the merge-base of HEAD and baseRef
//     (CI_BASE / --base; origin/<default branch> when empty), the same base
//     affected-package mode measures a change from;
//   - on the default branch, or with no base to measure from: `HEAD^..HEAD`,
//     the commit just made — every earlier one was scanned on the branch it
//     merged from;
//   - a first commit: "" (scan the whole history, which is that commit).
//
// `forge ci secret-scan --branch` scans this range, so the gate's secret-scan
// row judges only what the change adds: a project that pins the forge that
// added the row is never failed by it over a credential committed years ago.
func BranchRange(ctx context.Context, root, baseRef string) (rangeSpec, why string) {
	git := runGit
	head := func() (string, string) {
		if _, err := git(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD^"); err != nil {
			return "", "the first commit: the whole history"
		}
		return "HEAD^..HEAD", "the last commit"
	}
	defaultBranch := ""
	if baseRef == "" {
		baseRef = defaultBaseRef(ctx, git, root)
		if baseRef == "" {
			r, w := head()
			return r, w + " (no origin/<default branch> to measure a branch from)"
		}
		defaultBranch = strings.TrimPrefix(baseRef, "origin/")
	}
	if branch, err := git(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && defaultBranch != "" && branch == defaultBranch {
		r, w := head()
		return r, w + " (on the default branch " + branch + ")"
	}
	base, err := git(ctx, root, "merge-base", "HEAD", baseRef)
	if err != nil || base == "" {
		r, w := head()
		return r, w + " (no merge-base with " + baseRef + ")"
	}
	short := base
	if len(short) > 12 {
		short = short[:12]
	}
	return base + "..HEAD", "the commits since " + baseRef + " (" + short + ")"
}
