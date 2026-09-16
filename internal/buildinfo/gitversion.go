package buildinfo

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// Git-derived version for builds that carry no version of their own.
//
// THE CASE THIS EXISTS FOR is narrow, and worth stating precisely because the
// thing it replaced was applied far too broadly.
//
// Go stamps a real pseudo-version into build info for almost every build:
// `go install ...@v0.1.16` records the tag, `@main` records a pseudo-version,
// and a local `go build` records one too — even from a dirty tree
// (`v0.1.16-0.20260916085636-c01e07ec6ef2+dirty`). So Version() has a
// truthful answer already in all of those.
//
// It has none in exactly one shape: forge compiled INTO a host binary through
// a workspace. reliant `use`s ../forge in a go.work, so forge appears in
// reliant's build info as a dep at "(devel)" with no version, and the vcs.*
// settings describe RELIANT's tree, not forge's. Nothing in build info can
// name forge's version there.
//
// WHAT THIS REPLACED, and why it was wrong. That case used to fall back to
// the embedded VERSION file — the last RELEASED version — suffixed "+dev".
// The source is arbitrarily far AHEAD of that release, but semver ignores
// build metadata, so `v0.1.15+dev` compared EQUAL to a released `v0.1.15`.
// kclvendor's downgrade guard allows an equal-version overwrite, so a
// workspace build of main (containing a fix) and a released binary (without
// it) were indistinguishable — which is how forge#202's namespace-scoped
// operator ClusterRole/Binding scoping came within one `forge generate` of being reverted
// in control-plane. A floor that fabricates an ordering is worse than no
// version at all, because the ordering it invents is the wrong one.
//
// So this derives the REAL version from the checkout the binary was compiled
// from, in the same form Go itself would have produced, and returns "" rather
// than guessing when it cannot.

var (
	gitVersionOnce sync.Once
	gitVersionVal  string

	// gitVersionOverride is a test seam: these tests run inside a real forge
	// checkout, so the live derivation always succeeds and the "cannot
	// derive" path is otherwise unreachable.
	gitVersionOverride    string
	gitVersionOverrideSet bool
)

// SetGitVersion overrides what gitVersion returns. Test-only; pair with
// ClearGitVersion in a t.Cleanup.
func SetGitVersion(v string) {
	mu.Lock()
	defer mu.Unlock()
	gitVersionOverride = v
	gitVersionOverrideSet = true
}

// ClearGitVersion removes any override set by SetGitVersion.
func ClearGitVersion() {
	mu.Lock()
	defer mu.Unlock()
	gitVersionOverride = ""
	gitVersionOverrideSet = false
}

// gitVersion returns a Go-format pseudo-version for the forge checkout this
// binary was compiled from, or "" when it cannot be derived.
//
// Cached: it shells out to git, and Version() is called many times per run.
func gitVersion() string {
	mu.RLock()
	ov, ovSet := gitVersionOverride, gitVersionOverrideSet
	mu.RUnlock()
	if ovSet {
		return ov
	}
	gitVersionOnce.Do(func() { gitVersionVal = deriveGitVersion(forgeSourceRoot()) })
	return gitVersionVal
}

// forgeSourceRoot is the local forge checkout: the ldflags stamp when a
// contributor build supplied one, else recovered from this file's compiled
// path. Both are already how the go.work scaffold bridge finds it.
func forgeSourceRoot() string {
	if DevForgeRoot != "" {
		return DevForgeRoot
	}
	return DiscoverDevForgeRootFromSource()
}

// deriveGitVersion builds the pseudo-version. Split from gitVersion so it can
// be tested against a fixture repo instead of the live checkout.
//
// The FORM matters: Go's pseudo-version for a commit after tag vX.Y.Z is
// `vX.Y.(Z+1)-0.<utc timestamp>-<12-char sha>`, which sorts AFTER vX.Y.Z and
// BEFORE vX.Y.(Z+1). Anything else gets the ordering wrong in a way that
// matters — `git describe`'s natural `v0.1.15-23-gc01e07ec`, for instance,
// is a PRE-release of v0.1.15 and so sorts BEFORE the tag it is 23 commits
// ahead of.
func deriveGitVersion(root string) string {
	if root == "" {
		return ""
	}
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		// TZ is load-bearing: pseudo-version timestamps are UTC, and
		// %cd with a local-format date would otherwise use the machine's
		// zone and produce a version that differs between contributors.
		cmd.Env = append(cmd.Environ(), "TZ=UTC")
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	sha := run("rev-parse", "--short=12", "HEAD")
	ts := run("log", "-1", "--format=%cd", "--date=format-local:%Y%m%d%H%M%S")
	if sha == "" || ts == "" {
		return ""
	}

	base := "v0.0.0"
	if tag := run("describe", "--tags", "--abbrev=0", "--match", "v*"); tag != "" {
		if next := nextPatch(tag); next != "" {
			base = next
		}
	}

	v := fmt.Sprintf("%s-0.%s-%s", base, ts, sha)
	if !module.IsPseudoVersion(v) {
		// Refuse to emit something that only looks like a version.
		return ""
	}
	// A dirty tree is not the commit it claims to be. The suffix keeps
	// IsDevVersion and InstallableVersion honest about that (both key on
	// "+"), while semver still orders it with the commit.
	if run("status", "--porcelain") != "" {
		v += "+dirty"
	}
	return v
}

// nextPatch turns vX.Y.Z into vX.Y.(Z+1) — the base a pseudo-version for a
// commit AFTER that tag must carry. Returns "" for anything that is not a
// clean vX.Y.Z, including a tag that already has a prerelease: incrementing
// those correctly is ambiguous, and v0.0.0 is the honest fallback.
func nextPatch(tag string) string {
	if !semver.IsValid(tag) || semver.Prerelease(tag) != "" || semver.Build(tag) != "" {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(semver.Canonical(tag), "v"), ".")
	if len(parts) != 3 {
		return ""
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return ""
	}
	return fmt.Sprintf("v%s.%s.%d", parts[0], parts[1], patch+1)
}
