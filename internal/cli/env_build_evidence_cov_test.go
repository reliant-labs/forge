package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/bundle"
	"github.com/reliant-labs/forge/pkg/release"
)

func covDSharedOCI(uri, digest string) release.Artifact {
	return release.Artifact{
		Kind: release.KindOCI, Mode: release.ModeShared, URI: uri,
		Digests: map[string]string{release.SharedVariant: digest},
	}
}

func TestCovDReleaseImageRef(t *testing.T) {
	const d1, d2 = "sha256:aaaa", "sha256:bbbb"
	rel := release.Release{Artifacts: map[string]release.Artifact{
		"api":         covDSharedOCI("registry.example.com/shop/api:v1", d1),
		"worker":      covDSharedOCI("", d2),
		"duplicate-a": covDSharedOCI("registry.example.com/shop/dup", d1),
		"duplicate-b": covDSharedOCI("registry.example.com/shop/dup:other", d2),
		"site":        {Kind: release.KindFile, Mode: release.ModeShared, URI: "registry.example.com/shop/site", Digests: map[string]string{release.SharedVariant: d1}},
		"per-arch":    {Kind: release.KindOCI, Mode: release.ModeVariant, URI: "registry.example.com/shop/arch", Digests: map[string]string{"amd64": d1}},
	}}
	cases := []struct{ image, want string }{
		{"registry.example.com/shop/api", "registry.example.com/shop/api@" + d1},
		{"registry.example.com/shop/api:v9", "registry.example.com/shop/api@" + d1},
		{"worker", "worker@" + d2},
		{"registry.example.com/shop/dup", ""},
		{"registry.example.com/shop/site", ""},
		{"registry.example.com/shop/arch", ""},
		{"registry.example.com/shop/unknown", ""},
	}
	for _, c := range cases {
		if got := releaseImageRef(rel, c.image); got != c.want {
			t.Errorf("releaseImageRef(%q) = %q, want %q", c.image, got, c.want)
		}
	}
}

func TestCovDReleaseBuildSource(t *testing.T) {
	fallback := release.Source{Repo: "github.com/acme/render", Commit: strings.Repeat("1", 40)}
	built := release.Provenance{Repo: "github.com/acme/built", Commit: strings.Repeat("2", 40)}
	if got := releaseBuildSource(release.Release{Provenance: &built}, fallback); got.Repo != built.Repo || got.Commit != built.Commit {
		t.Errorf("a release's own provenance wins, got %+v", got)
	}
	for name, rel := range map[string]release.Release{
		"no provenance": {},
		"no repo":       {Provenance: &release.Provenance{Commit: strings.Repeat("2", 40)}},
		"short commit":  {Provenance: &release.Provenance{Repo: "r", Commit: "abc"}},
	} {
		if got := releaseBuildSource(rel, fallback); *got != fallback {
			t.Errorf("%s: got %+v, want the render's source", name, got)
		}
	}
}

func TestCovDSnapshotRuntimeRefs(t *testing.T) {
	api := release.ConsumerKey{Cluster: "c1", Workload: "api"}
	consumers := []release.Consumer{{Key: api, Slots: []string{"container:api", "container:sidecar", "init:migrate"}}}
	resource := func(owners []release.ConsumerKey, spec string) release.SnapshotResource {
		return release.SnapshotResource{Spec: []byte(spec), Owners: owners}
	}

	t.Run("deployment containers and init containers", func(t *testing.T) {
		got, err := snapshotRuntimeRefs([]release.SnapshotResource{resource([]release.ConsumerKey{api}, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  template:
    spec:
      containers:
      - {name: api, image: "r/api@sha256:1"}
      - {name: sidecar, image: "r/side@sha256:2"}
      - {name: undeclared, image: "r/other@sha256:3"}
      initContainers:
      - {name: migrate, image: "r/mig@sha256:4"}
`)}, consumers)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"container:api": "r/api@sha256:1", "container:sidecar": "r/side@sha256:2", "init:migrate": "r/mig@sha256:4"}
		if len(got[api]) != len(want) {
			t.Fatalf("refs = %v, want %v", got[api], want)
		}
		for slot, ref := range want {
			if got[api][slot] != ref {
				t.Errorf("slot %s = %q, want %q", slot, got[api][slot], ref)
			}
		}
	})

	t.Run("a cron job reads its job template and a pod its own spec", func(t *testing.T) {
		got, err := snapshotRuntimeRefs([]release.SnapshotResource{
			resource([]release.ConsumerKey{api}, `
apiVersion: batch/v1
kind: CronJob
metadata: {name: api}
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers: [{name: api, image: "r/cron@sha256:5"}]
`),
		}, consumers)
		if err != nil || got[api]["container:api"] != "r/cron@sha256:5" {
			t.Errorf("cron: %v %v", got, err)
		}
		got, err = snapshotRuntimeRefs([]release.SnapshotResource{
			resource([]release.ConsumerKey{api}, "apiVersion: v1\nkind: Pod\nmetadata: {name: api}\nspec:\n  containers: [{name: api, image: \"r/pod@sha256:6\"}]\n"),
		}, consumers)
		if err != nil || got[api]["container:api"] != "r/pod@sha256:6" {
			t.Errorf("pod: %v %v", got, err)
		}
	})

	t.Run("a managed Workload names its sole owner's primary slot", func(t *testing.T) {
		wl := "apiVersion: forge.dev/v1alpha1\nkind: Workload\nmetadata: {name: api}\nspec: {image: \"r/managed@sha256:7\"}\n"
		got, err := snapshotRuntimeRefs([]release.SnapshotResource{resource([]release.ConsumerKey{api}, wl)}, consumers)
		if err != nil || got[api]["container:api"] != "r/managed@sha256:7" {
			t.Errorf("one owner: %v %v", got, err)
		}
		got, err = snapshotRuntimeRefs([]release.SnapshotResource{resource([]release.ConsumerKey{api, {Cluster: "c2", Workload: "x"}}, wl)}, consumers)
		if err != nil || len(got) != 0 {
			t.Errorf("a shared Workload has no sole owner and binds nothing: %v %v", got, err)
		}
	})

	t.Run("a StaticSite binds its frontend release", func(t *testing.T) {
		site := "apiVersion: forge.dev/v1alpha1\nkind: StaticSite\nmetadata: {name: web}\nspec: {releaseRepository: \"r/site\", liveDigest: \"sha256:8\"}\n"
		got, err := snapshotRuntimeRefs([]release.SnapshotResource{{Spec: []byte(site), Key: release.ResourceKey{Cluster: "hosted"}}}, consumers)
		if err != nil {
			t.Fatal(err)
		}
		if got[release.ConsumerKey{Cluster: "hosted", Workload: "web"}]["frontend"] != "r/site@sha256:8" {
			t.Errorf("refs = %v", got)
		}
		incomplete := "apiVersion: forge.dev/v1alpha1\nkind: StaticSite\nmetadata: {name: web}\nspec: {releaseRepository: \"r/site\"}\n"
		got, err = snapshotRuntimeRefs([]release.SnapshotResource{{Spec: []byte(incomplete)}}, consumers)
		if err != nil || len(got) != 0 {
			t.Errorf("a StaticSite with no live digest binds nothing: %v %v", got, err)
		}
	})

	t.Run("two references for one slot are refused", func(t *testing.T) {
		dep := func(image string) release.SnapshotResource {
			return resource([]release.ConsumerKey{api}, "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api}\nspec:\n  template:\n    spec:\n      containers: [{name: api, image: \""+image+"\"}]\n")
		}
		_, err := snapshotRuntimeRefs([]release.SnapshotResource{dep("r/a@sha256:1"), dep("r/a@sha256:2")}, consumers)
		if err == nil || !strings.Contains(err.Error(), "api/container:api carries both r/a@sha256:1 and r/a@sha256:2") {
			t.Errorf("err = %v", err)
		}
		if _, err := snapshotRuntimeRefs([]release.SnapshotResource{dep("r/a@sha256:1"), dep("r/a@sha256:1")}, consumers); err != nil {
			t.Errorf("the same reference twice is consistent: %v", err)
		}
	})

	t.Run("an unparseable resource is an error", func(t *testing.T) {
		if _, err := snapshotRuntimeRefs([]release.SnapshotResource{{Spec: []byte("{not: [yaml")}}, consumers); err == nil {
			t.Error("want a parse error")
		}
	})
}

func TestCovDBuildEvidenceSnapshot_RefusesWhatCannotBeAttributed(t *testing.T) {
	cases := []struct {
		name string
		doc  envShapeDoc
		want string
	}{
		{"dirty checkout", envShapeDoc{Provenance: release.Provenance{Dirty: true, Repo: "r", Commit: strings.Repeat("a", 40)}}, "dirty checkout"},
		{"no repo", envShapeDoc{Provenance: release.Provenance{Commit: strings.Repeat("a", 40)}}, "no git source"},
		{"bad commit", envShapeDoc{Provenance: release.Provenance{Repo: "r", Commit: "abc"}}, "no git source"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := buildEvidenceSnapshot(context.Background(), t.TempDir(), "prod", c.doc, release.Release{Version: "v1"}, bundle.BuildInput{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestCovDAddEvidenceBinding(t *testing.T) {
	key := release.ConsumerKey{Cluster: "c1", Workload: "api"}
	art := release.SnapshotArtifact{Kind: release.KindOCI, Location: "r/api", Content: "sha256:" + strings.Repeat("a", 64)}
	snap := &release.Snapshot{Artifacts: map[release.ArtifactID]release.SnapshotArtifact{}}
	if err := addEvidenceBinding(snap, key, "container:api", art, "v1"); err != nil {
		t.Fatal(err)
	}
	if len(snap.Artifacts) != 1 || len(snap.Bindings) != 1 || snap.Bindings[0].Tag != "v1" || snap.Bindings[0].Slot != "container:api" || snap.Bindings[0].Consumer != key {
		t.Fatalf("snapshot = %+v", snap)
	}
	if err := addEvidenceBinding(snap, key, "container:sidecar", art, ""); err != nil {
		t.Fatal(err)
	}
	if len(snap.Artifacts) != 1 || len(snap.Bindings) != 2 {
		t.Errorf("the same artifact bound to a second slot is stored once: %+v", snap)
	}

	external := art
	external.Origin.External = &release.ExternalArtifact{}
	if err := addEvidenceBinding(snap, key, "container:other", external, ""); err == nil || !strings.Contains(err.Error(), "is both built and external") {
		t.Errorf("an artifact cannot be both built and external, got %v", err)
	}
	if err := addEvidenceBinding(snap, key, "x", release.SnapshotArtifact{Kind: release.KindOCI, Location: "r/x", Content: "not-a-digest"}, ""); err == nil {
		t.Error("an artifact with no identity must be refused")
	}
}
