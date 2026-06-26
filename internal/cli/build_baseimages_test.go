package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/baseimage"
	"github.com/reliant-labs/forge/internal/config"
)

// resetBaseArgsCache clears the per-build memoization so each test sees a
// fresh lock read. The cache is keyed by projectDir; tests use distinct temp
// dirs, but clearing keeps them independent of order.
func resetBaseArgsCache() {
	baseArgsMu.Lock()
	baseArgsCache = map[string][]string{}
	baseArgsMu.Unlock()
}

func cfgWithBases(prefix string, tags ...string) *config.ProjectConfig {
	return &config.ProjectConfig{
		Docker: config.DockerConfig{
			BaseImages: config.BaseImagesConfig{MirrorPrefix: prefix, Tags: tags},
		},
	}
}

// writeLockForTest pins the declared bases via a fake resolver and writes the
// lock under dir, returning the lock so callers can assert against its refs.
func writeLockForTest(t *testing.T, dir, prefix string, tags ...string) *baseimage.Lock {
	t.Helper()
	d := baseimage.Declared{MirrorPrefix: prefix, Tags: tags}
	digests := map[string]string{}
	for _, tag := range tags {
		digests[baseimage.MirrorTagRef(prefix, tag)] = "sha256:" + baseimage.Slug(tag)
	}
	lk, err := baseimage.Repin(context.Background(), d, fixedResolver{digests})
	if err != nil {
		t.Fatalf("Repin: %v", err)
	}
	if _, err := baseimage.WriteLock(dir, lk); err != nil {
		t.Fatalf("WriteLock: %v", err)
	}
	return lk
}

type fixedResolver struct{ digests map[string]string }

func (f fixedResolver) Resolve(_ context.Context, ref string) (string, error) {
	return f.digests[ref], nil
}

// TestAppendBaseImageBuildArgs_InjectsLockedRefs is the build-wiring proof:
// with a lock present, every locked base flows into the docker arg list as a
// `--build-arg BASE_<slug>=<mirror-ref>@<digest>`.
func TestAppendBaseImageBuildArgs_InjectsLockedRefs(t *testing.T) {
	resetBaseArgsCache()
	dir := t.TempDir()
	prefix := "us-docker.pkg.dev/p/dockerhub"
	writeLockForTest(t, dir, prefix, "alpine:3.21", "golang:1.26-alpine")
	cfg := cfgWithBases(prefix, "alpine:3.21", "golang:1.26-alpine")

	got := appendBaseImageBuildArgs([]string{"build"}, cfg, dir)

	// Expect: build, --build-arg BASE_ALPINE_3_21=…, --build-arg BASE_GOLANG_1_26_ALPINE=…
	joined := strings.Join(got, " ")
	wantAlpine := "BASE_ALPINE_3_21=" + prefix + "/library/alpine@sha256:ALPINE_3_21"
	wantGolang := "BASE_GOLANG_1_26_ALPINE=" + prefix + "/library/golang@sha256:GOLANG_1_26_ALPINE"
	if !strings.Contains(joined, wantAlpine) {
		t.Errorf("missing alpine build-arg in %q", joined)
	}
	if !strings.Contains(joined, wantGolang) {
		t.Errorf("missing golang build-arg in %q", joined)
	}
	// Two bases → two --build-arg flags appended.
	if n := strings.Count(joined, "--build-arg"); n != 2 {
		t.Errorf("--build-arg count: got %d, want 2 (%q)", n, joined)
	}
}

// TestAppendBaseImageBuildArgs_NoLockIsNoop confirms the build path is unchanged
// when base_images are declared but no lock exists yet (falls back to the
// Dockerfile ARG defaults).
func TestAppendBaseImageBuildArgs_NoLockIsNoop(t *testing.T) {
	resetBaseArgsCache()
	dir := t.TempDir()
	cfg := cfgWithBases("m/p", "alpine:3.21")
	got := appendBaseImageBuildArgs([]string{"build"}, cfg, dir)
	if len(got) != 1 || got[0] != "build" {
		t.Errorf("no-lock should be a no-op, got %v", got)
	}
}

// TestAppendBaseImageBuildArgs_FeatureOff confirms a project that declares no
// base_images sees zero injection.
func TestAppendBaseImageBuildArgs_FeatureOff(t *testing.T) {
	resetBaseArgsCache()
	got := appendBaseImageBuildArgs([]string{"build"}, &config.ProjectConfig{}, t.TempDir())
	if len(got) != 1 {
		t.Errorf("feature off should be a no-op, got %v", got)
	}
}

// TestEnforceBaseImagesFresh_InSyncPasses confirms a lock that pins exactly the
// declared tag set through the declared mirror is NOT treated as stale.
func TestEnforceBaseImagesFresh_InSyncPasses(t *testing.T) {
	dir := t.TempDir()
	prefix := "us-docker.pkg.dev/p/dockerhub"
	writeLockForTest(t, dir, prefix, "alpine:3.21")
	cfg := cfgWithBases(prefix, "alpine:3.21")
	if err := enforceBaseImagesFresh(cfg, dir, false); err != nil {
		t.Fatalf("in-sync lock should pass, got %v", err)
	}
}

// TestEnforceBaseImagesFresh_StaleTagSetErrors is the core of the friction fix:
// when forge.yaml declares a base the lock doesn't pin, a normal build is a HARD
// error (not a warning), so stale pins can't ship silently. The message must be
// actionable — name the lock, point at --repin-bases and --force-stale-bases.
func TestEnforceBaseImagesFresh_StaleTagSetErrors(t *testing.T) {
	dir := t.TempDir()
	prefix := "us-docker.pkg.dev/p/dockerhub"
	// Lock pins only alpine; forge.yaml now also declares golang → drift.
	writeLockForTest(t, dir, prefix, "alpine:3.21")
	cfg := cfgWithBases(prefix, "alpine:3.21", "golang:1.26-alpine")

	err := enforceBaseImagesFresh(cfg, dir, false)
	if err == nil {
		t.Fatal("stale tag set should be a hard error without --force-stale-bases")
	}
	if !errors.Is(err, errStaleBaseImages) {
		t.Errorf("error should wrap errStaleBaseImages, got %v", err)
	}
	for _, want := range []string{baseimage.LockRel, "--repin-bases", "--force-stale-bases"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %v", want, err)
		}
	}
}

// TestEnforceBaseImagesFresh_StaleMirrorErrors confirms a moved mirror prefix
// (same tags, different mirror) is also caught as drift.
func TestEnforceBaseImagesFresh_StaleMirrorErrors(t *testing.T) {
	dir := t.TempDir()
	writeLockForTest(t, dir, "old-mirror.example/p/dockerhub", "alpine:3.21")
	cfg := cfgWithBases("new-mirror.example/p/dockerhub", "alpine:3.21")
	if err := enforceBaseImagesFresh(cfg, dir, false); !errors.Is(err, errStaleBaseImages) {
		t.Fatalf("moved mirror should be stale, got %v", err)
	}
}

// TestEnforceBaseImagesFresh_ForceProceeds confirms --force-stale-bases turns
// the hard error back into a non-fatal proceed.
func TestEnforceBaseImagesFresh_ForceProceeds(t *testing.T) {
	dir := t.TempDir()
	prefix := "us-docker.pkg.dev/p/dockerhub"
	writeLockForTest(t, dir, prefix, "alpine:3.21")
	cfg := cfgWithBases(prefix, "alpine:3.21", "golang:1.26-alpine")
	if err := enforceBaseImagesFresh(cfg, dir, true); err != nil {
		t.Fatalf("--force-stale-bases should proceed, got %v", err)
	}
}

// TestEnforceBaseImagesFresh_MissingLockPasses confirms "declared but not yet
// pinned" is NOT an error — that path falls back to the Dockerfile ARG defaults
// (themselves pinned). It's a hint, not drift.
func TestEnforceBaseImagesFresh_MissingLockPasses(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgWithBases("m/p", "alpine:3.21")
	if err := enforceBaseImagesFresh(cfg, dir, false); err != nil {
		t.Fatalf("missing lock should pass (not yet pinned), got %v", err)
	}
}

// TestEnforceBaseImagesFresh_FeatureOffPasses confirms a project declaring no
// base_images is never gated.
func TestEnforceBaseImagesFresh_FeatureOffPasses(t *testing.T) {
	if err := enforceBaseImagesFresh(&config.ProjectConfig{}, t.TempDir(), false); err != nil {
		t.Fatalf("feature off should pass, got %v", err)
	}
}

// TestBaseImageBuildEnv_ForExternalBuilds confirms the external build_cmd path
// gets the same pinned refs as BASE_<slug> env/substitution tokens.
func TestBaseImageBuildEnv_ForExternalBuilds(t *testing.T) {
	resetBaseArgsCache()
	dir := t.TempDir()
	prefix := "us-docker.pkg.dev/p/dockerhub"
	writeLockForTest(t, dir, prefix, "ubuntu:24.04")
	cfg := cfgWithBases(prefix, "ubuntu:24.04")

	env := baseImageBuildEnv(cfg, dir)
	want := prefix + "/library/ubuntu@sha256:UBUNTU_24_04"
	if env["BASE_UBUNTU_24_04"] != want {
		t.Errorf("BASE_UBUNTU_24_04: got %q, want %q", env["BASE_UBUNTU_24_04"], want)
	}
}

// TestMergeBuildEnv_ServiceWins confirms a service's explicit BuildEnv overlays
// the injected base refs.
func TestMergeBuildEnv_ServiceWins(t *testing.T) {
	base := map[string]string{"BASE_X": "from-lock", "SHARED": "lock"}
	over := map[string]string{"SHARED": "svc", "OWN": "svc"}
	got := mergeBuildEnv(base, over)
	if got["BASE_X"] != "from-lock" || got["SHARED"] != "svc" || got["OWN"] != "svc" {
		t.Errorf("mergeBuildEnv precedence wrong: %v", got)
	}
	if mergeBuildEnv(nil, nil) != nil {
		t.Error("mergeBuildEnv(nil,nil) should be nil")
	}
}
