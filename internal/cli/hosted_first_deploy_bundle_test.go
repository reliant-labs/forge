package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

// cutHostedFixtureRelease cuts a release pinning every hosted artifact of the
// golden hosted env, with NO promotion bound — the state of a first deploy.
func cutHostedFixtureRelease(t *testing.T, dir string, e *KCLEntities, version string) {
	t.Helper()
	group, err := buildHostedGroup("prod", e)
	if err != nil || group == nil {
		t.Fatalf("buildHostedGroup: %v", err)
	}
	artifacts := map[string]release.Artifact{}
	for _, svc := range group.Services {
		w := svc.Hosted
		if w != nil && (w.Tier == deploytarget.HostedTierWorkload || w.Tier == deploytarget.HostedTierStatic) {
			artifacts[deploytarget.HostedArtifactKey(svc)] = release.Artifact{
				Kind: release.KindOCI, Mode: release.ModeShared,
				Digests: map[string]string{release.SharedVariant: "sha256:" + strings.Repeat("b", 64)},
			}
		}
	}
	if _, err := testReleases(t, dir).Cut(context.Background(), release.Release{Version: version, Artifacts: artifacts}); err != nil {
		t.Fatalf("cut release: %v", err)
	}
}

// A first hosted deploy has a release but no promotion. The bundle is pinned
// by the release it names, so it is never sealed over placeholder digests.
func TestHostedBundleObjectsPinToTheNamedReleaseBeforeAnyPromotion(t *testing.T) {
	e, _ := loadContract(t, "hosted")
	dir := t.TempDir()
	cutHostedFixtureRelease(t, dir, e, "v0.1.0")

	_, placeholder, err := hostedBundleObjects(context.Background(), dir, "prod", e)
	if err != nil || !placeholder {
		t.Fatalf("without a named release: placeholder=%v err=%v; want placeholder", placeholder, err)
	}
	ctx := withHostedPinRelease(context.Background(), "v0.1.0")
	objs, placeholder, err := hostedBundleObjects(ctx, dir, "prod", e)
	if err != nil {
		t.Fatalf("hostedBundleObjects: %v", err)
	}
	if placeholder || len(objs) == 0 {
		t.Fatalf("pinned by v0.1.0: placeholder=%v objects=%d; want a real pin", placeholder, len(objs))
	}
}

// Deploying a named release that has no bundle yet projects and records one.
func TestEnsureHostedReleaseBundleWritesWhenMissing(t *testing.T) {
	e, _ := loadContract(t, "hosted")
	dir := t.TempDir()
	cutHostedFixtureRelease(t, dir, e, "v0.1.0")
	ledger := testLedger(t, dir)
	ledger.Hosted = true

	var got []bundleBuildInputs
	prev := writeBundlesFn
	writeBundlesFn = func(_ context.Context, _ string, _ []string, in bundleBuildInputs) ([]bundleWriteOutcome, error) {
		got = append(got, in)
		return nil, nil
	}
	t.Cleanup(func() { writeBundlesFn = prev })

	ensureHostedReleaseBundle(context.Background(), dir, "prod", "v0.1.0", ledger, io.Discard)
	if len(got) != 1 || got[0].Release != "v0.1.0" || len(got[0].Pins.Images) == 0 {
		t.Fatalf("writeBundles calls = %+v; want one for v0.1.0 carrying the release's pins", got)
	}

	ensureHostedReleaseBundle(context.Background(), dir, "prod", "v9", ledger, io.Discard)
	if len(got) != 1 {
		t.Fatalf("an uncut release must not write a bundle; calls = %d", len(got))
	}
}
