package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/ledgerfile"
	"github.com/reliant-labs/forge/pkg/release"
)

// An unavailable identity lookup still permits fresh cut refs, but never
// borrows an existing index's mappings, whether bound or legacy.
func TestSBOMUnresolvedTargetDoesNotReplayIndex(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"foreign-target", ""} {
		t.Run(target, func(t *testing.T) {
			fake := &fakeVulnService{features: []string{hostedVulnFeatureSBOMUpload}, noIdentity: true}
			hs, r, warn := hostedSBOMCut(t, fake, "api", "worker")
			store, _ := hs.sboms()
			if err := recordTargetSBOMIndex(store, r, target); err != nil {
				t.Fatal(err)
			}
			worker := r.Artifacts["worker"]
			worker.SBOM = nil
			r.Artifacts["worker"] = worker
			hs.recordReleaseSBOMs(context.Background(), r)
			if len(fake.records) != 1 || fake.records[0]["artifactName"] != "api" {
				t.Fatalf("unresolved target replayed indexed SBOM: %+v", fake.records)
			}
			if !strings.Contains(warn.String(), "could not resolve SBOM target identity") {
				t.Fatalf("missing identity warning: %s", warn)
			}
		})
	}
}

func TestSBOMTargetSwitchReplacesProjectForRetries(t *testing.T) {
	t.Parallel()
	for _, viaPush := range []bool{false, true} {
		t.Run(map[bool]string{false: "cut", true: "push"}[viaPush], func(t *testing.T) {
			fake := &fakeVulnService{features: []string{hostedVulnFeatureSBOMUpload}}
			hs, r, warn := hostedSBOMCut(t, fake, "api")
			store, _ := hs.sboms()
			r.Project = "project-a"
			if err := recordTargetSBOMIndex(store, r, "target-a"); err != nil {
				t.Fatal(err)
			}
			r.Project = "project-b"
			run := sbomPushRun{Version: r.Version, Project: r.Project, Target: "target-b", CutAt: r.CreatedAt, Artifacts: r.Artifacts}
			if !viaPush {
				if err := recordTargetSBOMIndex(store, r, run.Target); err != nil {
					t.Fatal(err)
				}
			}
			setSBOMUploadState(store, hs.endpoint, run, nil, ledgerfile.SBOMUploadPending, "retry")
			pending, _, err := store.PendingSBOMUploads(hs.endpoint)
			if err != nil || len(pending) != 1 || pending[0].Project != "project-b" {
				t.Fatalf("stale retry project: %+v, %v", pending, err)
			}
			hs.drainPendingSBOMs(context.Background(), sbomUploader{client: hs.client, endpoint: hs.endpoint}, store, pending, warn)
			if len(fake.records) != 1 || fake.records[0]["project"] != "project-b" {
				t.Fatalf("retry addressed old project: %+v", fake.records)
			}
		})
	}
}

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
