package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/statefile"
)

// BuildState records what `forge build --push` actually pushed to a
// registry, so a subsequent `forge deploy <env>` can reference the
// same tag even when the working tree has changed between phases.
//
// The original bug this struct closes: `forge build` tags an image
// `<reg>/<svc>:<git-describe>` (which includes `-dirty` when the
// working tree has untracked or modified files), then `forge deploy`
// independently computes a tag — and the two diverge whenever a
// working-tree mutation between the two phases flips the dirty bit,
// or when the two phases use different git commands altogether
// (build uses `git describe --tags --always --dirty`, deploy used
// `git rev-parse --short HEAD`). The state file fixes both by making
// build authoritative.
//
// Wire format is JSON; fields use snake_case for readability when a
// user peeks at the file by hand. PushedAt is RFC3339 so a human can
// eyeball "how stale is this?" without a parser.
type BuildState struct {
	Image    string `json:"image"`
	Tag      string `json:"tag"`
	Registry string `json:"registry"`
	// Pushed is true when the image was pushed to Registry. False for
	// local/scp/compose builds — the image lives only on the build host.
	// Recording the handoff no longer depends on a push (that gate left
	// non-registry deploys with no tag to read); push just adds the
	// registry coordinates.
	Pushed bool `json:"pushed"`
	// Git provenance of the build, so `forge deploy` can warn when it's
	// about to ship a non-reproducible (dirty / untagged) build. Commit
	// is the full HEAD sha; GitTag is the exact tag on HEAD (empty when
	// HEAD isn't tagged); Dirty is true when the working tree had
	// uncommitted changes at build time.
	Commit string `json:"commit,omitempty"`
	GitTag string `json:"git_tag,omitempty"`
	Dirty  bool   `json:"dirty,omitempty"`
	// PushedAt is the wall-clock build time, formatted as time.RFC3339.
	// The state file is informational across forge invocations, so we use
	// real time here — reproducibility constraints don't apply.
	PushedAt string `json:"pushed_at"`
	// Digest is the content-addressed manifest digest of the pushed image,
	// in the canonical `sha256:...` form (no `@` prefix, no repo). Captured
	// from the registry after `docker push` succeeds (see imageRepoDigest).
	// EMPTY for non-pushed builds (local/scp/compose — the image lives only
	// on the build host with no registry manifest to address) and for any
	// build where the digest lookup failed (capture is best-effort and never
	// fails the build).
	//
	// When present, `forge deploy` pins the manifest to `<image>@<Digest>`
	// instead of the mutable `:Tag` — a digest can't go stale and can't be
	// re-pointed, so the node-cache / re-tag-didn't-take failure class is
	// structurally impossible. When empty, deploy falls back to the tag, so
	// every non-pushed transport keeps working unchanged.
	Digest string `json:"digest,omitempty"`
	// Platforms is the set of OS/arch platforms the pushed image advertises
	// (e.g. ["linux/amd64"], or both for a multi-arch index), captured from
	// the registry manifest alongside Digest. Informational today (a human
	// can eyeball what arch shipped); the deploy preflight inspects the live
	// image's arch independently. Empty when the lookup failed or the build
	// wasn't pushed.
	Platforms []string `json:"platforms,omitempty"`
}

// imageRepoDigest reads the content-addressed manifest digest of a pushed
// image ref from the registry, returning the canonical `sha256:...` form
// (no `@`, no repo prefix) plus the platforms the manifest advertises.
//
// It prefers `docker buildx imagetools inspect`, which queries the REGISTRY
// (so it reports the manifest-list / index digest for a multi-arch push and
// the platform set), and falls back to `docker inspect`'s RepoDigests (a
// local-daemon read) when buildx is unavailable. Best-effort by contract:
// any failure returns ("", nil, err) and the caller records no digest and
// proceeds — a missing digest only costs the tag-fallback deploy path, never
// the build.
func imageRepoDigest(ctx context.Context, ref string) (digest string, platforms []string, err error) {
	// buildx imagetools inspect hits the registry and prints the top-level
	// manifest digest (the index digest for a multi-arch push). The raw
	// format avoids parsing the human table. Platforms come from a second,
	// equally cheap format pass.
	digCmd := exec.CommandContext(ctx, "docker", "buildx", "imagetools", "inspect", ref,
		"--format", "{{.Manifest.Digest}}")
	if out, derr := digCmd.Output(); derr == nil {
		if d := strings.TrimSpace(string(out)); strings.HasPrefix(d, "sha256:") {
			platforms = imageToolsPlatforms(ctx, ref)
			return d, platforms, nil
		}
	}
	// Fallback: `docker inspect` reads RepoDigests off the LOCAL daemon —
	// "<repo>@sha256:...". Match the entry for this ref's repo so we don't
	// pick a stale digest from another tag of a different repo.
	out, ierr := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{range .RepoDigests}}{{println .}}{{end}}", ref).Output()
	if ierr != nil {
		return "", nil, ierr
	}
	repo := refRepo(ref)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		at := strings.LastIndex(line, "@")
		if at < 0 {
			continue
		}
		if refRepo(line[:at]) == repo {
			return line[at+1:], nil, nil
		}
	}
	return "", nil, fmt.Errorf("no RepoDigest for %s", ref)
}

// imageToolsPlatforms returns the OS/arch platforms a registry manifest
// advertises (one entry per platform in a multi-arch index, a single entry
// for a single-platform image). Best-effort: returns nil on any failure so a
// digest is still recorded without it.
func imageToolsPlatforms(ctx context.Context, ref string) []string {
	out, err := exec.CommandContext(ctx, "docker", "buildx", "imagetools", "inspect", ref,
		"--format", "{{range .Manifest.Manifests}}{{.Platform.OS}}/{{.Platform.Architecture}}\n{{end}}").Output()
	if err != nil {
		return nil
	}
	var platforms []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p := strings.TrimSpace(line)
		// A single-platform image has no .Manifest.Manifests list, so the
		// range yields nothing; "unknown/unknown" attestation entries (buildx
		// provenance) are dropped — they aren't runnable platforms.
		if p == "" || p == "/" || strings.Contains(p, "unknown") {
			continue
		}
		if !seen[p] {
			seen[p] = true
			platforms = append(platforms, p)
		}
	}
	return platforms
}

// refRepo strips an image ref down to its repository, dropping any `:tag`
// or `@digest` suffix while preserving a `registry:port/` host. The tag/
// digest separator is the rightmost `:`/`@` AFTER the rightmost `/`, so a
// registry-port colon (`localhost:5051/foo`) is never mistaken for a tag.
func refRepo(ref string) string {
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	slash := strings.LastIndex(ref, "/")
	tail := ref
	if slash >= 0 {
		tail = ref[slash+1:]
	}
	if colon := strings.LastIndex(tail, ":"); colon >= 0 {
		tail = tail[:colon]
	}
	if slash >= 0 {
		return ref[:slash+1] + tail
	}
	return tail
}

// gitBuildProvenance captures the HEAD commit, an exact tag on HEAD (if
// any), and whether the working tree is dirty — recorded into BuildState
// so deploy can flag non-reproducible builds. Best-effort: missing git
// yields zero values, never an error (the build already succeeded).
func gitBuildProvenance(ctx context.Context) (commit, gitTag string, dirty bool) {
	if out, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output(); err == nil {
		commit = strings.TrimSpace(string(out))
	}
	if out, err := exec.CommandContext(ctx, "git", "describe", "--tags", "--exact-match").Output(); err == nil {
		gitTag = strings.TrimSpace(string(out))
	}
	if out, err := exec.CommandContext(ctx, "git", "status", "--porcelain").Output(); err == nil {
		dirty = strings.TrimSpace(string(out)) != ""
	}
	return commit, gitTag, dirty
}

// buildStatePath returns the absolute path to the per-env build-state
// file. One file per environment so `forge build --push --env=dev`
// and `forge build --push --env=staging` don't clobber each other,
// and so `forge deploy <env>` can read the right one without a
// separate lookup. When env is empty we use the literal "default"
// segment to keep the path stable.
func buildStatePath(projectDir, env string) string {
	if env == "" {
		env = "default"
	}
	return statefile.Path(projectDir, "build-"+env+".json")
}

// WriteBuildState persists a successful `forge build --push` to disk.
// Called by runBuild after every per-image push succeeds, so the most
// recent push is always the source of truth a subsequent
// `forge deploy <env>` consumes.
//
// The directory is created lazily — projects that never use --push
// never grow a .forge/state/ tree. File is 0o644 (world-readable) to
// match the other .forge state files' mode; nothing in here is secret.
func WriteBuildState(projectDir, env string, state BuildState) error {
	return statefile.Write(buildStatePath(projectDir, env), "build state", state)
}

// ReadBuildState loads the per-env build-state file. Returns
// (nil, nil) when the file is missing — that's the
// "deploy-without-build" path (CI with a separate build job, or the
// user running `forge deploy` on a fresh checkout) and the caller
// falls through to resolveImageTag. Returns (nil, err) for malformed
// JSON or unreadable files; callers should not silently swallow these
// because they mean the state file exists but can't be trusted.
func ReadBuildState(projectDir, env string) (*BuildState, error) {
	return statefile.Read[BuildState](buildStatePath(projectDir, env), "build state")
}

// nowRFC3339 returns the current wall-clock time formatted as
// time.RFC3339. Wrapped so tests can verify the timestamp shape
// without re-deriving the format string.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
