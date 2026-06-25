package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildtarget"
)

func TestBuildStateLookupEnvs(t *testing.T) {
	cases := []struct {
		env  string
		want []string
	}{
		{"prod", []string{"prod", "default"}},
		{"staging", []string{"staging", "default"}},
		{"", []string{"default"}},
		{"default", []string{"default"}},
	}
	for _, c := range cases {
		if got := buildStateLookupEnvs(c.env); !reflect.DeepEqual(got, c.want) {
			t.Errorf("buildStateLookupEnvs(%q) = %v, want %v", c.env, got, c.want)
		}
	}
}

// TestResolveDeployImageTag_DefaultFallback proves the handoff that the
// --push-gate fix enables: `forge build --docker` (no --env) writes the
// "default" record, and `forge deploy prod` reads it back via the fallback
// when there is no prod-specific record.
func TestResolveDeployImageTag_DefaultFallback(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBuildState(dir, "default", BuildState{Tag: "cutover-v61", Image: "app", Pushed: false}); err != nil {
		t.Fatalf("write default state: %v", err)
	}

	tag, _, src, err := resolveDeployImageTag(context.Background(), dir, "prod", "", false)
	if err != nil {
		t.Fatalf("resolveDeployImageTag: %v", err)
	}
	if tag != "cutover-v61" {
		t.Fatalf("tag = %q, want cutover-v61 (default fallback not used)", tag)
	}
	if src == "" {
		t.Fatalf("source should name the default build-state file, got empty")
	}
}

// TestResolveDeployImageTag_EnvWinsOverDefault: a prod-specific record
// takes precedence over the default one.
func TestResolveDeployImageTag_EnvWinsOverDefault(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBuildState(dir, "default", BuildState{Tag: "from-default"}); err != nil {
		t.Fatalf("write default: %v", err)
	}
	if err := WriteBuildState(dir, "prod", BuildState{Tag: "from-prod"}); err != nil {
		t.Fatalf("write prod: %v", err)
	}
	tag, _, _, err := resolveDeployImageTag(context.Background(), dir, "prod", "", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tag != "from-prod" {
		t.Fatalf("tag = %q, want from-prod (env-specific should win)", tag)
	}
}

// TestResolveDeployImageTag_FlagOverridesAll: --tag bypasses build-state.
func TestResolveDeployImageTag_FlagOverridesAll(t *testing.T) {
	dir := t.TempDir()
	_ = WriteBuildState(dir, "prod", BuildState{Tag: "from-state"})
	tag, _, src, err := resolveDeployImageTag(context.Background(), dir, "prod", "explicit", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tag != "explicit" {
		t.Fatalf("tag = %q, want explicit", tag)
	}
	if src != "explicit --tag flag" {
		t.Fatalf("src = %q, want explicit flag", src)
	}
}

// TestResolveDeployImageTag_KeepsTagWhenDigestCaptured proves the
// per-image-digest redesign: resolveDeployImageTag ALWAYS returns the
// mutable tag for BOTH imageRef and plainTag, even when the build state
// captured a digest. Digest pinning moved to resolveDeployImageDigests (a
// PER-IMAGE map) so each service pins ITS OWN image's digest — the old
// behaviour returned ONE env-wide digest here and stamped it onto every
// image (a reliant pinned to the control-plane digest → manifest unknown).
func TestResolveDeployImageTag_KeepsTagWhenDigestCaptured(t *testing.T) {
	dir := t.TempDir()
	digest := "sha256:" + strings.Repeat("a", 64)
	if err := WriteBuildState(dir, "prod", BuildState{
		Tag: "v1.4.0", Image: "control-plane", Digest: digest,
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}
	ref, plain, _, err := resolveDeployImageTag(context.Background(), dir, "prod", "", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ref != "v1.4.0" {
		t.Errorf("imageRef = %q, want v1.4.0 (the tag — digest no longer rides imageRef)", ref)
	}
	if plain != "v1.4.0" {
		t.Errorf("plainTag = %q, want v1.4.0", plain)
	}
}

// TestResolveDeployImageDigests_PerImage is THE regression guard: two images
// (control-plane via the aggregate build-state, reliant via the per-service
// external-build state) must each resolve to THEIR OWN digest in the map —
// never collapsed to one env-wide digest. workspace-base proves a second
// distinct external image lands too, and a service SHARING an image carries
// the same digest.
func TestResolveDeployImageDigests_PerImage(t *testing.T) {
	dir := t.TempDir()
	cpDigest := "sha256:" + strings.Repeat("c", 64)
	reliantDigest := "sha256:" + strings.Repeat("1", 64)
	wsDigest := "sha256:" + strings.Repeat("8", 64)

	// Aggregate build state (docker PROJECT path) → control-plane digest.
	if err := WriteBuildState(dir, "staging", BuildState{
		Tag: "staging", Image: "control-plane", Digest: cpDigest,
	}); err != nil {
		t.Fatalf("write aggregate state: %v", err)
	}
	// Per-service external-build states → reliant (shared by two services)
	// and workspace-base, each its OWN digest.
	for _, st := range []buildtarget.State{
		{Service: "reliant-api-server", Image: "reliant", Tag: "staging", Digest: reliantDigest},
		{Service: "reliant-temporal-worker", Image: "reliant", Tag: "staging", Digest: reliantDigest},
		{Service: "workspace-base", Image: "workspace-base", Tag: "staging", Digest: wsDigest},
	} {
		if err := buildtarget.WriteState(dir, "staging", st); err != nil {
			t.Fatalf("write per-service state %s: %v", st.Service, err)
		}
	}

	got, err := resolveDeployImageDigests(dir, "staging", false)
	if err != nil {
		t.Fatalf("resolveDeployImageDigests: %v", err)
	}
	want := map[string]string{
		"control-plane":  cpDigest,
		"reliant":        reliantDigest,
		"workspace-base": wsDigest,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d image digests, want %d: %v", len(got), len(want), got)
	}
	for img, wd := range want {
		if got[img] != wd {
			t.Errorf("image %q digest = %q, want %q", img, got[img], wd)
		}
	}
	// THE regression: reliant must NOT inherit control-plane's digest.
	if got["reliant"] == got["control-plane"] {
		t.Errorf("reliant digest equals control-plane digest (%q) — the env-wide-digest bug", got["reliant"])
	}
}

// TestResolveDeployImageDigests_NoDigestFlag proves --no-digest yields an
// empty map (every image stays on its tag).
func TestResolveDeployImageDigests_NoDigestFlag(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBuildState(dir, "staging", BuildState{
		Tag: "staging", Image: "control-plane", Digest: "sha256:" + strings.Repeat("c", 64),
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}
	got, err := resolveDeployImageDigests(dir, "staging", true)
	if err != nil {
		t.Fatalf("resolveDeployImageDigests: %v", err)
	}
	if got != nil {
		t.Errorf("--no-digest should yield a nil map, got %v", got)
	}
}

// TestResolveDeployImageTag_FallsBackToTagWhenNoDigest proves the safe
// fallback: a build state with NO digest (third-party images, non-pushed
// builds, the local-registry e2e path) keeps deploying by the tag, unchanged.
func TestResolveDeployImageTag_FallsBackToTagWhenNoDigest(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBuildState(dir, "prod", BuildState{Tag: "v1.4.0", Image: "app"}); err != nil {
		t.Fatalf("write state: %v", err)
	}
	ref, plain, _, err := resolveDeployImageTag(context.Background(), dir, "prod", "", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ref != "v1.4.0" || plain != "v1.4.0" {
		t.Errorf("imageRef=%q plainTag=%q, want both v1.4.0 (tag fallback)", ref, plain)
	}
}

// TestResolveDeployImageTag_NoDigestFlagForcesTag proves the --no-digest
// escape hatch: even with a captured digest, the operator can opt back to the
// mutable tag.
func TestResolveDeployImageTag_NoDigestFlagForcesTag(t *testing.T) {
	dir := t.TempDir()
	digest := "sha256:" + strings.Repeat("b", 64)
	if err := WriteBuildState(dir, "prod", BuildState{
		Tag: "v1.4.0", Image: "app", Digest: digest,
	}); err != nil {
		t.Fatalf("write state: %v", err)
	}
	ref, _, _, err := resolveDeployImageTag(context.Background(), dir, "prod", "", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ref != "v1.4.0" {
		t.Errorf("imageRef = %q, want v1.4.0 (--no-digest forces the tag)", ref)
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA("5fe39fe57ea80e970eba"); got != "5fe39fe57ea8" {
		t.Errorf("shortSHA long = %q", got)
	}
	if got := shortSHA(""); got != "unknown" {
		t.Errorf("shortSHA empty = %q, want unknown", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA short = %q", got)
	}
}
