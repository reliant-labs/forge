package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// resolveImageTag computes the canonical docker-image tag that `forge
// build --push` would push and that `forge deploy <env>` would reference
// when no explicit override and no per-env build-state file is present.
//
// The tag mirrors what dockerBuildProject actually tags the image with:
// `git describe --tags --always --dirty`. This is the cornerstone of
// the "build is the source of truth" contract — both phases compute the
// same string from the same git state, so a build followed by a deploy
// in a clean tree always agrees.
//
// When the working tree changes between phases (the original bug:
// untracked files appear/disappear, `-dirty` toggles), only the state
// file written by `forge build --push` keeps the two phases in lock-
// step. resolveImageTag is the standalone-deploy fallback for when no
// state file exists.
//
// The env arg is reserved for future per-env tag conventions (e.g.
// staging vs prod) and is currently unused. Keeping it in the signature
// avoids a churn-only change later.
//
// Returns an error only when git is reachable but produces no output —
// in practice this means HEAD is unset (empty repo). When git itself
// fails (not a git repo, git not installed), the empty string is
// returned and the caller decides whether to surface or default.
func resolveImageTag(ctx context.Context, _ string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		// Not a git repo, or git not on PATH. Fall back to short SHA;
		// rev-parse covers the "git installed but no tag" case which
		// describe --always already handled, plus the "shallow clone
		// with no tags" path that some CI runners hit.
		out2, err2 := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output()
		if err2 != nil {
			return "", fmt.Errorf("git tag resolution: %w", err)
		}
		tag := strings.TrimSpace(string(out2))
		if tag == "" {
			return "", fmt.Errorf("git rev-parse --short HEAD returned empty output")
		}
		return tag, nil
	}
	tag := strings.TrimSpace(string(out))
	if tag == "" {
		return "", fmt.Errorf("git describe returned empty output")
	}
	return tag, nil
}
