package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildtarget"
)

// TestReleaseFileStem_PreservesDots pins Fix 3: a semver version's literal
// label IS its filename stem (dots are filesystem-safe), so "v1.0.0" maps
// to "v1.0.0", NOT the surprising "v1_0_0" the old statefile.SafeSegment
// produced. Path separators and other unsafe bytes still flatten to '_',
// and a dot-only traversal token is neutralized.
func TestReleaseFileStem_PreservesDots(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v1.0.0", "v1.0.0"}, // the Fix 3 case: dots preserved, least surprise
		{"v1.4.0", "v1.4.0"}, //
		{"1.2.3-rc.1", "1.2.3-rc.1"},
		{"v2", "v2"},   // no dots, unchanged
		{"a/b", "a_b"}, // path separator flattened — never escapes the dir
		{`a\b`, "a_b"}, // backslash too
		{"a b", "a_b"}, // space flattened
		{".", "_"},     // bare dot is a dir ref, neutralized
		{"..", "__"},   // parent-dir traversal neutralized
		{"...", "___"}, //
		{"", "_"},      // empty never yields an empty stem
	}
	for _, c := range cases {
		if got := releaseFileStem(c.in); got != c.want {
			t.Errorf("releaseFileStem(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReleasePath_VersionMappingRoundTrips is the cross-command consistency
// guard for Fix 3: build (WriteRelease), promote/deploy (ReadRelease), and
// the path the CLI prints all resolve a version label to the SAME file via
// releasePath. A release written under "v1.0.0" must read back under
// "v1.0.0", and the file on disk must be the non-surprising "v1.0.0.json".
func TestReleasePath_VersionMappingRoundTrips(t *testing.T) {
	dir := t.TempDir()
	const version = "v1.0.0"

	rel := Release{
		Version:   version,
		CreatedAt: nowRFC3339(),
		Artifacts: map[string]ReleaseArtifact{
			"control-plane": {Mode: "shared", Digests: map[string]string{"*": sha("a")}},
		},
	}
	// build writes...
	if err := WriteRelease(dir, rel); err != nil {
		t.Fatalf("WriteRelease: %v", err)
	}
	// ...the on-disk file is the LITERAL version (dots preserved), not v1_0_0.
	wantPath := filepath.Join(dir, ".forge/releases", "v1.0.0.json")
	if got := releasePath(dir, version); got != wantPath {
		t.Errorf("releasePath = %q, want %q", got, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("expected ledger at %q (literal version): %v", wantPath, err)
	}
	// promote/deploy read the SAME version back through the SAME mapping.
	got, err := ReadRelease(dir, version)
	if err != nil || got == nil {
		t.Fatalf("ReadRelease(%q) = (%v, %v), want a ledger", version, got, err)
	}
	if got.Version != version {
		t.Errorf("round-tripped version = %q, want %q", got.Version, version)
	}
}

func sha(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// TestRelease_LedgerRoundTrip locks the on-disk contract: a Release written by
// `forge build --release` reads back intact, including the per-image shared
// digest map and platforms.
func TestRelease_LedgerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Release{
		Version:   "v1.4.0",
		Git:       ReleaseGit{Commit: "8a7be2b", Tag: "v1.4.0", Dirty: false},
		CreatedAt: nowRFC3339(),
		Artifacts: map[string]ReleaseArtifact{
			"control-plane": {Mode: "shared", Digests: map[string]string{"*": sha("a")}, Platforms: []string{"linux/amd64"}},
			"reliant":       {Mode: "shared", Digests: map[string]string{"*": sha("b")}},
		},
	}
	if err := WriteRelease(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadRelease(dir, "v1.4.0")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil {
		t.Fatal("read returned nil")
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("round-trip mismatch:\n  want %+v\n  got  %+v", want, *got)
	}
}

// TestReadRelease_MissingIsNilNil keeps "no such release" distinct from an
// error so promote can produce a friendly not-found message.
func TestReadRelease_MissingIsNilNil(t *testing.T) {
	got, err := ReadRelease(t.TempDir(), "v9.9.9")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

// TestHarvestReleaseArtifacts proves the release ledger is a projection of the
// EXISTING digest capture: the aggregate project build state AND the
// per-service external-build states both feed the harvested artifact map, each
// image carrying its OWN captured digest.
func TestHarvestReleaseArtifacts(t *testing.T) {
	dir := t.TempDir()
	// Aggregate project image (what dockerBuildProject captures), written under
	// the env-agnostic "default" key a plain `forge build --release` produces.
	if err := WriteBuildState(dir, "default", BuildState{
		Image: "control-plane", Tag: "v1.4.0", Pushed: true, PushedAt: nowRFC3339(),
		Digest: sha("a"), Platforms: []string{"linux/amd64"},
	}); err != nil {
		t.Fatalf("write aggregate: %v", err)
	}
	// Per-service external-build states (reliant, workspace-base) keyed to the
	// SAME env the harvest runs against (empty env → "default").
	for img, c := range map[string]string{"reliant": "b", "workspace-base": "d"} {
		if err := buildtarget.WriteState(dir, "default", buildtarget.State{
			Service: img, Image: img, Tag: "v1.4.0", PushedAt: nowRFC3339(), Digest: sha(c),
		}); err != nil {
			t.Fatalf("write per-service %s: %v", img, err)
		}
	}

	// Empty env harvests the "default" records (buildStateLookupEnvs("") == ["default"]).
	got := harvestReleaseArtifacts(dir, "")
	want := map[string]ReleaseArtifact{
		"control-plane":  {Mode: "shared", Digests: map[string]string{"*": sha("a")}, Platforms: []string{"linux/amd64"}},
		"reliant":        {Mode: "shared", Digests: map[string]string{"*": sha("b")}},
		"workspace-base": {Mode: "shared", Digests: map[string]string{"*": sha("d")}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("harvest mismatch:\n  want %+v\n  got  %+v", want, got)
	}
}

// TestHarvestReleaseArtifacts_SkipsDigestless confirms an image with no
// captured digest is omitted — a release records only content-addressed bytes.
func TestHarvestReleaseArtifacts_SkipsDigestless(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBuildState(dir, "default", BuildState{
		Image: "control-plane", Tag: "v1.4.0", PushedAt: nowRFC3339(), // no Digest
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := harvestReleaseArtifacts(dir, ""); len(got) != 0 {
		t.Errorf("want empty (no digests captured), got %+v", got)
	}
}

// TestResolveReleaseDigests flattens shared artifacts to the image→digest map
// the deploy/promote paths consume, and errors when a release pins nothing.
func TestResolveReleaseDigests(t *testing.T) {
	rel := Release{
		Version: "v1.4.0",
		Artifacts: map[string]ReleaseArtifact{
			"control-plane": {Mode: "shared", Digests: map[string]string{"*": sha("a")}},
			"reliant":       {Mode: "shared", Digests: map[string]string{"*": sha("b")}},
		},
	}
	got, err := resolveReleaseDigests(rel)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := map[string]string{"control-plane": sha("a"), "reliant": sha("b")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolve mismatch:\n  want %+v\n  got  %+v", want, got)
	}

	if _, err := resolveReleaseDigests(Release{Version: "v0", Artifacts: map[string]ReleaseArtifact{}}); err == nil {
		t.Error("want error for a release with no shared digests, got nil")
	}
}

// TestResolveReleaseDigests_EmptyArtifactsActionableError pins the friction
// fix: a release cut with an EMPTY artifact map (forgot --push, no services
// built, all external builds skipped) must fail with an ACTIONABLE message that
// names the version, the likely causes, and `forge audit` — not the old vague
// "carries no shared image digests to pin" that read like an internal invariant.
func TestResolveReleaseDigests_EmptyArtifactsActionableError(t *testing.T) {
	_, err := resolveReleaseDigests(Release{Version: "v1.4.0", Artifacts: map[string]ReleaseArtifact{}})
	if err == nil {
		t.Fatal("want error for a release with no artifacts, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		`"v1.4.0"`, // names the offending release
		"no image digests",
		"--push",      // the most common cause/remedy
		"build_cwd",   // external-build-skip cause
		"forge audit", // the inspect next-step
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("empty-artifacts error missing %q\n  got: %s", want, msg)
		}
	}
}

// TestResolveReleaseDigests_VariantOnlyError confirms a release that carries
// artifacts but only variant-mode ones (no shared digest the MVP can pin)
// fails with a message distinct from the empty-artifacts case, so the user
// isn't told to re-run --push when the real situation is "variant promotion
// isn't supported yet".
func TestResolveReleaseDigests_VariantOnlyError(t *testing.T) {
	_, err := resolveReleaseDigests(Release{
		Version: "v2.0.0",
		Artifacts: map[string]ReleaseArtifact{
			// variant-mode: a digest keyed by an env variant, NOT sharedVariantKey,
			// so SharedDigest() returns ("", false).
			"control-plane": {Mode: "variant", Digests: map[string]string{"prod": sha("a")}},
		},
	})
	if err == nil {
		t.Fatal("want error for a variant-only release, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "variant") {
		t.Errorf("variant-only error should mention variant artifacts\n  got: %s", msg)
	}
	if strings.Contains(msg, "--push") {
		t.Errorf("variant-only error should NOT suggest --push (wrong remedy)\n  got: %s", msg)
	}
}

// TestRunPromote_EmptyReleaseSurfacesActionableError proves the friction fix
// reaches the user through the actual `forge promote` entrypoint: promoting a
// release that was cut with no digests fails (no binding written) with the
// actionable guidance, not a silent/confusing dead end.
func TestRunPromote_EmptyReleaseSurfacesActionableError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := WriteRelease(dir, Release{
		Version:   "v3.1.0",
		CreatedAt: nowRFC3339(),
		Artifacts: map[string]ReleaseArtifact{}, // cut with no digests
	}); err != nil {
		t.Fatalf("write release: %v", err)
	}

	err := runPromote("v3.1.0", "staging")
	if err == nil {
		t.Fatal("want error promoting an empty release, got nil")
	}
	if !strings.Contains(err.Error(), "forge audit") {
		t.Errorf("promote error should carry the actionable guidance\n  got: %s", err.Error())
	}

	// No binding should have been written for a release that pins nothing.
	if _, bound, berr := boundReleaseForEnv(dir, "staging"); berr != nil {
		t.Fatalf("read binding: %v", berr)
	} else if bound {
		t.Error("staging must NOT be bound when the release pins no digests")
	}
}

// TestEnvReleases_RoundTrip + MissingDefaultsEmpty lock the binding ledger
// contract: a missing file is a usable empty ledger, not an error.
func TestEnvReleases_RoundTripAndMissing(t *testing.T) {
	dir := t.TempDir()

	er, err := ReadEnvReleases(dir)
	if err != nil {
		t.Fatalf("read missing: %v", err)
	}
	if er == nil || er.Bindings == nil || len(er.Bindings) != 0 {
		t.Fatalf("missing ledger should be empty-but-usable, got %+v", er)
	}

	er.Bindings["prod"] = EnvBinding{Release: "v1.4.0", Resolved: map[string]string{"control-plane": sha("a")}, PromotedAt: nowRFC3339()}
	if err := WriteEnvReleases(dir, *er); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadEnvReleases(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(got.Bindings, er.Bindings) {
		t.Errorf("binding round-trip mismatch:\n  want %+v\n  got  %+v", er.Bindings, got.Bindings)
	}
}

// TestResolveDeployDigests_BoundEnvUsesRelease proves the load-bearing
// precedence: when an env is bound to a release, the deploy resolves the
// digests from the RELEASE binding — NOT the per-env build state — so a
// promoted env ships the release's bytes.
func TestResolveDeployDigests_BoundEnvUsesRelease(t *testing.T) {
	dir := t.TempDir()

	// Per-env build state holds a DIFFERENT (older) digest for control-plane.
	if err := WriteBuildState(dir, "prod", BuildState{
		Image: "control-plane", Tag: "old", Pushed: true, PushedAt: nowRFC3339(), Digest: sha("0"),
	}); err != nil {
		t.Fatalf("write build state: %v", err)
	}
	// prod is promoted to v1.4.0, whose control-plane digest is sha(a).
	er, _ := ReadEnvReleases(dir)
	er.Bindings["prod"] = EnvBinding{
		Release:    "v1.4.0",
		Resolved:   map[string]string{"control-plane": sha("a"), "reliant": sha("b")},
		PromotedAt: nowRFC3339(),
	}
	if err := WriteEnvReleases(dir, *er); err != nil {
		t.Fatalf("write bindings: %v", err)
	}

	digests, boundRel, err := resolveDeployDigests(dir, "prod", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if boundRel != "v1.4.0" {
		t.Errorf("boundRelease = %q, want v1.4.0", boundRel)
	}
	// The RELEASE digest wins over the per-env build-state digest.
	if digests["control-plane"] != sha("a") {
		t.Errorf("control-plane = %q, want release digest %q (release must override per-env build state)", digests["control-plane"], sha("a"))
	}
	if digests["reliant"] != sha("b") {
		t.Errorf("reliant = %q, want %q", digests["reliant"], sha("b"))
	}
}

// TestResolveDeployDigests_UnboundEnvFallsBack proves full backward compat: an
// env with NO release binding resolves exactly the per-env build-state digests,
// with an empty bound release — byte-identical to the pre-release flow.
func TestResolveDeployDigests_UnboundEnvFallsBack(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBuildState(dir, "staging", BuildState{
		Image: "control-plane", Tag: "v1.4.0", Pushed: true, PushedAt: nowRFC3339(), Digest: sha("c"),
	}); err != nil {
		t.Fatalf("write build state: %v", err)
	}

	digests, boundRel, err := resolveDeployDigests(dir, "staging", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if boundRel != "" {
		t.Errorf("boundRelease = %q, want empty (no binding)", boundRel)
	}
	if digests["control-plane"] != sha("c") {
		t.Errorf("control-plane = %q, want per-env build-state digest %q", digests["control-plane"], sha("c"))
	}
}

// TestResolveDeployDigests_NoDigestSkipsRelease confirms --no-digest disables
// the release lookup too (the tag-only escape hatch overrides everything).
func TestResolveDeployDigests_NoDigestSkipsRelease(t *testing.T) {
	dir := t.TempDir()
	er, _ := ReadEnvReleases(dir)
	er.Bindings["prod"] = EnvBinding{Release: "v1.4.0", Resolved: map[string]string{"control-plane": sha("a")}, PromotedAt: nowRFC3339()}
	if err := WriteEnvReleases(dir, *er); err != nil {
		t.Fatalf("write bindings: %v", err)
	}

	digests, boundRel, err := resolveDeployDigests(dir, "prod", true /* noDigest */)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if boundRel != "" || len(digests) != 0 {
		t.Errorf("--no-digest should yield no digests and no bound release, got digests=%v release=%q", digests, boundRel)
	}
}

// TestRunPromote_WritesBinding exercises the promote command end-to-end against
// a release ledger, proving it records the env→release binding with the
// resolved digests snapshotted from the release.
func TestRunPromote_WritesBinding(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// A release ledger exists (as `forge build --release` would have written).
	if err := WriteRelease(dir, Release{
		Version:   "v1.4.0",
		CreatedAt: nowRFC3339(),
		Artifacts: map[string]ReleaseArtifact{
			"control-plane": {Mode: "shared", Digests: map[string]string{"*": sha("a")}},
			"reliant":       {Mode: "shared", Digests: map[string]string{"*": sha("b")}},
		},
	}); err != nil {
		t.Fatalf("write release: %v", err)
	}

	if err := runPromote("v1.4.0", "staging"); err != nil {
		t.Fatalf("promote: %v", err)
	}

	binding, bound, err := boundReleaseForEnv(dir, "staging")
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if !bound {
		t.Fatal("staging should be bound after promote")
	}
	if binding.Release != "v1.4.0" {
		t.Errorf("binding.Release = %q, want v1.4.0", binding.Release)
	}
	want := map[string]string{"control-plane": sha("a"), "reliant": sha("b")}
	if !reflect.DeepEqual(binding.Resolved, want) {
		t.Errorf("binding.Resolved = %+v, want %+v", binding.Resolved, want)
	}
}

// TestRunPromote_UnknownReleaseErrors confirms promoting a release that was
// never cut fails with a clear not-found rather than writing a bad binding.
func TestRunPromote_UnknownReleaseErrors(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := runPromote("v9.9.9", "staging"); err == nil {
		t.Fatal("want error promoting a non-existent release, got nil")
	}
}
