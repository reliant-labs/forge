package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/ledgerfile"
	"github.com/reliant-labs/forge/pkg/release"
)

func TestSBOMIndexKeepsAcceptedReleaseAfterRejectedRebuild(t *testing.T) {
	t.Parallel()
	fake := &fakeVulnService{features: []string{hostedVulnFeatureSBOMUpload}}
	hs, a, _ := hostedSBOMCut(t, fake, "api")
	if _, err := hs.Cut(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b := a
	b.Artifacts = map[string]release.Artifact{}
	art := a.Artifacts["api"]
	art.Digests = map[string]string{release.SharedVariant: sha("e")}
	art.SBOM = &release.SBOMRef{Digest: sha("f"), Status: release.SBOMStatusOK}
	b.Artifacts["api"] = art
	if _, err := hs.Cut(context.Background(), b); err == nil {
		t.Fatal("conflicting rebuild accepted")
	}
	store, _ := hs.sboms()
	// Even an attempted re-index must leave an established mapping intact.
	if err := recordSBOMIndex(store, b); err != nil {
		t.Fatal(err)
	}
	idx, err := store.ReleaseSBOMIndex(a.Version)
	if err != nil {
		t.Fatal(err)
	}
	var meta *sbomMetadataIndex
	remote := a.Artifacts["api"]
	remote.SBOM = nil
	got := planSBOMPush("api", sbomPushRun{Artifacts: map[string]release.Artifact{"api": remote}}, idx, store, &meta)
	if got.SBOMDigest != a.Artifacts["api"].SBOM.Digest {
		t.Fatalf("selected rejected SBOM: %+v", got)
	}
}

func TestSBOMRetryDoesNotCrossTargetsOrAdoptLegacyMappings(t *testing.T) {
	t.Parallel()
	fake := &fakeVulnService{features: []string{}}
	hs, r, _ := hostedSBOMCut(t, fake, "api")
	if _, err := hs.Cut(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	store, _ := hs.sboms()
	idx, _ := store.ReleaseSBOMIndex(r.Version)
	if idx.Target == "" {
		t.Fatal("accepted cut has no target")
	}
	// Another org on the same endpoint already has a pending version with
	// exactly the same artifact name. It must never be drained by this caller.
	for _, target := range []string{"other-org/project", ""} {
		if err := store.UpdateReleaseSBOMIndex("v0.9.0", func(x *ledgerfile.ReleaseSBOMIndex) error {
			x.Target = target
			x.Artifacts = idx.Artifacts
			x.Uploads = map[string]ledgerfile.SBOMUpload{hs.endpoint: {State: ledgerfile.SBOMUploadPending}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		art := r.Artifacts["api"]
		art.SBOM = nil
		// pushReleaseSBOMs applies the target filter before planning.
		out := pushReleaseSBOMs(context.Background(), sbomUploader{}, store, sbomPushRun{Version: "v0.9.0", Target: idx.Target, Artifacts: map[string]release.Artifact{"api": art}})
		if len(out.Artifacts) != 1 || out.Artifacts[0].Status != sbomPushMissing {
			t.Fatalf("foreign mapping used: %+v", out)
		}
		// Restore the foreign pending entry: the explicit planning check
		// above wrote its own outcome, but the automatic drain must see
		// the original foreign target and pending state.
		if err := store.UpdateReleaseSBOMIndex("v0.9.0", func(x *ledgerfile.ReleaseSBOMIndex) error {
			x.Target = target
			x.Artifacts = idx.Artifacts
			x.Uploads = map[string]ledgerfile.SBOMUpload{hs.endpoint: {State: ledgerfile.SBOMUploadPending}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		hs.vulnCaps = nil
		fake.features = []string{hostedVulnFeatureSBOMUpload}
		next := r
		next.Version = "v1.1." + strings.Repeat("0", len(target)%2+1)
		if _, err := hs.Cut(context.Background(), next); err != nil {
			t.Fatal(err)
		}
		for _, rec := range fake.records {
			if rec["version"] == "v0.9.0" {
				t.Fatal("foreign release replayed")
			}
		}
	}
}
