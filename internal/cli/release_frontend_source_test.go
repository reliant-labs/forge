package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/gitsource"
)

// A release is supposed to be "the environment at a version". Container
// images are only the containerized half of one: a Firebase Hosting SPA
// declared via forge.GitSource builds from a pinned commit and uploads to a
// CDN, so it produces no image and writes no build state. Before this it was
// therefore absent from the ledger entirely — no promotion record, no
// staleness check, nothing to verify — while its only pin was a ref string in
// KCL that nothing reconciled. These tests pin the two halves of the fix: the
// frontend IS captured, and a release that misses a declared artifact FAILS
// rather than shipping an environment with a hole in it.

// fakeFrontendEntity builds a source-pinned FrontendEntity for the tests.
func fakeFrontendEntity(name, repo, ref, subdir string) FrontendEntity {
	return FrontendEntity{
		Name:   name,
		Type:   "vite",
		Source: &config.GitSource{Repo: repo, Ref: ref, Subdir: subdir},
		Deploy: &FrontendDeployEntity{Type: "firebase"},
	}
}

// TestAddFrontendSourceArtifacts_CapturesPinnedFrontend is the core of the
// gap: a source-pinned frontend becomes a source-mode artifact keyed by
// frontend name, carrying the repo/ref/subdir it was declared with and the
// commit that ref resolved to.
func TestAddFrontendSourceArtifacts_CapturesPinnedFrontend(t *testing.T) {
	const wantCommit = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"

	entities := &KCLEntities{
		Frontends: []FrontendEntity{
			fakeFrontendEntity("reliant-web", "github.com/reliant-labs/reliant", "v1.7.12", "web"),
		},
	}

	artifacts := map[string]ReleaseArtifact{
		"control-plane": {Mode: artifactModeShared, Digests: map[string]string{sharedVariantKey: sha("a")}},
	}

	resolver := stubPinResolver{commit: wantCommit, dir: "/cache/reliant/web"}
	if err := addFrontendSourceArtifactsWith(context.Background(), resolver, entities, artifacts); err != nil {
		t.Fatalf("addFrontendSourceArtifacts: %v", err)
	}

	art, ok := artifacts["reliant-web"]
	if !ok {
		t.Fatalf("reliant-web missing from the ledger; got keys %v", releaseArtifactKeys(artifacts))
	}
	if art.Mode != artifactModeSource {
		t.Errorf("mode = %q, want %q", art.Mode, artifactModeSource)
	}
	if art.Source == nil {
		t.Fatal("source artifact carries no Source pin")
	}
	if art.Source.Repo != "github.com/reliant-labs/reliant" {
		t.Errorf("repo = %q", art.Source.Repo)
	}
	if art.Source.Ref != "v1.7.12" {
		t.Errorf("ref = %q, want v1.7.12", art.Source.Ref)
	}
	if art.Source.Subdir != "web" {
		t.Errorf("subdir = %q, want web", art.Source.Subdir)
	}
	// The commit is the half that makes the release auditable: a ref alone
	// is not content-addressed, because a moved tag silently changes what
	// the release means.
	if art.Source.Commit != wantCommit {
		t.Errorf("commit = %q, want %q", art.Source.Commit, wantCommit)
	}
	// The image artifact is untouched.
	if _, ok := artifacts["control-plane"]; !ok {
		t.Error("image artifact was dropped")
	}
}

// TestCheckReleaseCoversEnv_FailsOnMissingFrontend is the guard that replaces
// a hand-maintained image list. The env DECLARES reliant-web; a ledger
// without it must fail the cut rather than promote an environment whose
// frontend silently stays on whatever the previous binding held.
func TestCheckReleaseCoversEnv_FailsOnMissingFrontend(t *testing.T) {
	entities := &KCLEntities{
		Services: []ServiceEntity{{Name: "admin-server", Image: "control-plane"}},
		Frontends: []FrontendEntity{
			fakeFrontendEntity("reliant-web", "github.com/reliant-labs/reliant", "v1.7.12", "web"),
		},
	}
	artifacts := map[string]ReleaseArtifact{
		"control-plane": {Mode: artifactModeShared, Digests: map[string]string{sharedVariantKey: sha("a")}},
	}

	err := checkReleaseCoversEnv(entities, artifacts, buildOptions{release: "v1.6.0", env: "prod"})
	if err == nil {
		t.Fatal("expected a cut with a missing declared frontend to FAIL, got nil")
	}
	if !strings.Contains(err.Error(), "reliant-web") {
		t.Errorf("error should name the missing artifact, got: %v", err)
	}
}

// TestCheckReleaseCoversEnv_FailsOnMissingImage is the same guard for the
// container half — the case the old workflow approximated with "at least one
// image resolved". This is strictly stronger: EVERY declared image must be
// present, not merely one of them.
func TestCheckReleaseCoversEnv_FailsOnMissingImage(t *testing.T) {
	entities := &KCLEntities{
		Services: []ServiceEntity{
			{Name: "admin-server", Image: "control-plane"},
			{Name: "workspace-base", Image: "workspace-base"},
		},
	}
	artifacts := map[string]ReleaseArtifact{
		"control-plane": {Mode: artifactModeShared, Digests: map[string]string{sharedVariantKey: sha("a")}},
	}

	err := checkReleaseCoversEnv(entities, artifacts, buildOptions{release: "v1.6.0", env: "prod"})
	if err == nil {
		t.Fatal("expected a cut missing a declared image to FAIL, got nil")
	}
	if !strings.Contains(err.Error(), "workspace-base") {
		t.Errorf("error should name the missing image, got: %v", err)
	}
}

// TestCheckReleaseCoversEnv_PassesWhenComplete is the negative control: the
// gate must not fire on a release that genuinely covers the env, or it would
// just be a wall.
func TestCheckReleaseCoversEnv_PassesWhenComplete(t *testing.T) {
	entities := &KCLEntities{
		Services: []ServiceEntity{{Name: "admin-server", Image: "control-plane"}},
		Frontends: []FrontendEntity{
			fakeFrontendEntity("reliant-web", "github.com/reliant-labs/reliant", "v1.7.12", "web"),
		},
	}
	artifacts := map[string]ReleaseArtifact{
		"control-plane": {Mode: artifactModeShared, Digests: map[string]string{sharedVariantKey: sha("a")}},
		"reliant-web": {
			Mode:   artifactModeSource,
			Source: &ReleaseSource{Repo: "github.com/reliant-labs/reliant", Ref: "v1.7.12", Commit: "abc123"},
		},
	}

	if err := checkReleaseCoversEnv(entities, artifacts, buildOptions{release: "v1.6.0", env: "prod"}); err != nil {
		t.Fatalf("complete release should pass the gate, got: %v", err)
	}
}

// TestResolveReleaseDigests_SkipsSourceArtifacts pins the boundary between the
// two artifact kinds. A source artifact has no registry digest and must never
// leak into the image→digest map a manifest pins from — a frontend name in
// there would render as an image reference that does not exist.
func TestResolveReleaseDigests_SkipsSourceArtifacts(t *testing.T) {
	rel := Release{
		Version: "v1.6.0",
		Artifacts: map[string]ReleaseArtifact{
			"control-plane": {Mode: artifactModeShared, Digests: map[string]string{sharedVariantKey: sha("a")}},
			"reliant-web": {
				Mode:   artifactModeSource,
				Source: &ReleaseSource{Repo: "github.com/reliant-labs/reliant", Ref: "v1.7.12", Commit: "abc123"},
			},
		},
	}

	digests, err := resolveReleaseDigests(rel)
	if err != nil {
		t.Fatalf("resolveReleaseDigests: %v", err)
	}
	if _, leaked := digests["reliant-web"]; leaked {
		t.Error("source artifact leaked into the image digest map; a manifest would pin a nonexistent image")
	}
	if digests["control-plane"] != sha("a") {
		t.Errorf("image digest = %q", digests["control-plane"])
	}

	// The same release must still surface the frontend on the source side,
	// so a promotion records it.
	sources := resolveReleaseSources(rel)
	if got, ok := sources["reliant-web"]; !ok || got.Commit != "abc123" {
		t.Errorf("resolveReleaseSources lost the frontend pin: %+v", sources)
	}
	if _, leaked := sources["control-plane"]; leaked {
		t.Error("image artifact leaked into the source map")
	}
}

// TestAddFrontendSourceArtifacts_RejectsLocalOverride pins a correctness rule
// that is easy to get wrong by being helpful. A source override is a
// developer saying "build what is in front of me" — a working tree, not a
// pin. Recording it in a release would produce a ledger claiming a version
// that no other machine can reproduce, which is worse than failing: the
// release LOOKS reproducible and is not. So the cut fails and says so.
func TestAddFrontendSourceArtifacts_RejectsLocalOverride(t *testing.T) {
	entities := &KCLEntities{
		Frontends: []FrontendEntity{
			fakeFrontendEntity("reliant-web", "github.com/reliant-labs/reliant", "v1.7.12", "web"),
		},
	}
	artifacts := map[string]ReleaseArtifact{}

	resolver := stubPinResolver{dir: "/Users/dev/src/reliant/web", overridden: true}
	err := addFrontendSourceArtifactsWith(context.Background(), resolver, entities, artifacts)
	if err == nil {
		t.Fatal("expected a cut against a local override to FAIL, got nil")
	}
	if !strings.Contains(err.Error(), "OVERRIDE") {
		t.Errorf("error should name the override as the cause, got: %v", err)
	}
	if _, recorded := artifacts["reliant-web"]; recorded {
		t.Error("an overridden source must NOT be frozen into the ledger")
	}
}

// stubPinResolver stands in for the gitsource resolver so the release-capture
// logic is testable without a network or a git binary.
type stubPinResolver struct {
	commit     string
	dir        string
	overridden bool
	err        error
}

func (s stubPinResolver) Resolve(_ context.Context, src gitsource.Source) (gitsource.Resolution, error) {
	if s.err != nil {
		return gitsource.Resolution{}, s.err
	}
	return gitsource.Resolution{
		Dir:        s.dir,
		Commit:     s.commit,
		Overridden: s.overridden,
	}, nil
}

func releaseArtifactKeys(m map[string]ReleaseArtifact) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
