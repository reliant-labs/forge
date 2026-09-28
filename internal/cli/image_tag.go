package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// resolveImageTag computes the canonical docker-image tag that `forge
// build --push` would push and that `forge env deploy <env>` would reference
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

// envImageTagFor returns the env's RESOLVED image tag for a given
// (registry-less) image name. This is the tag `forge env deploy <env>`
// references for that image, so `forge build <env>` defaults its build tag to
// it — build and deploy then push/pull the SAME tag by construction.
//
// A workload built into that image whose resolved spec.image carries a tag
// answers with it (a per-workload pin wins: `image = "reliant:e2e"` renders
// spec.image `<registry>/reliant:e2e` and artifact `reliant`, so the build is
// IMAGE=reliant TAG=e2e); otherwise the env's own resolved image_tag
// (`output.image_tag`). A digest-pinned spec.image carries no tag and falls
// through to the env tag — the render refuses a digest on a workload forge
// builds, so this is only a digest `forge env deploy` pinned from a previous
// build of the same image.
//
// Returns "" when entities is nil (no --env / KCL render failed) or the name
// is empty — every such case falls the caller back to git-derived tagging.
func envImageTagFor(entities *KCLEntities, image string) string {
	if entities == nil || image == "" {
		return ""
	}
	for _, w := range entities.Workloads {
		if w.Image != image {
			continue
		}
		if tag, ok := pinnedTagOf(w.Spec.Image, image); ok {
			return tag
		}
	}
	return entities.ImageTag
}

// pinnedTagOf is the tag of a resolved ref when that ref names the artifact
// repository — `localhost:5051/acme/app:v2` for `acme/app` answers v2. The
// repository must end the ref's path at a `/` boundary, so `app` does not
// claim `myapp:v1`'s tag (the org path is kept on both sides; the registry
// prefix is whatever the target resolved). ok=false for a digest, a tagless
// ref, or a different repository.
func pinnedTagOf(ref, repository string) (tag string, ok bool) {
	if strings.Contains(ref, "@") {
		return "", false
	}
	lastColon := strings.LastIndex(ref, ":")
	if lastColon < 0 || strings.Contains(ref[lastColon+1:], "/") {
		return "", false
	}
	path, tag := ref[:lastColon], ref[lastColon+1:]
	if tag == "" || (path != repository && !strings.HasSuffix(path, "/"+repository)) {
		return "", false
	}
	return tag, true
}
