package bundle

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

func placementBuildInput() BuildInput {
	return BuildInput{Project: "p", Env: "prod", CreatedAt: time.Unix(1700000000, 0).UTC(), Provenance: release.Provenance{Repo: "r", Commit: strings.Repeat("c", 40), Tree: strings.Repeat("0", 40), ForgeVersion: "v"}, Placements: []PlacementDeclaration{{Workload: "api", Runtime: "cluster", Actuator: "forge"}, {Workload: "web", Runtime: "hosted", Actuator: "platform", Dependencies: []string{"api"}}}, Shape: ShapeInput{Kind: release.EnvPersistent, Manifests: `# cluster: prod
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: app
  labels:
    forge.dev/workload: api
spec:
  replicas: 1
  selector:
    matchLabels: {app: api}
  template:
    metadata:
      labels: {app: api}
    spec:
      containers:
        - name: api
          image: api:v1
---
# cluster: prod
apiVersion: batch/v1
kind: Job
metadata:
  name: migrate-abcd1234
  namespace: app
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: migrate
          image: migrator:v1
---
# cluster: prod
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
  namespace: app
data: {mode: production}
---
# cluster: hosted
apiVersion: forge.dev/v1alpha1
kind: StaticSite
metadata:
  name: web
spec: {}
`}}
}

func TestPlacementPrerequisitesSameParseBindings(t *testing.T) {
	in := placementBuildInput()
	built, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	p := built.Doc.PlacementPrerequisites
	if p == nil || len(p.Items) != 2 || len(p.Edges) != 1 {
		t.Fatalf("graph %+v", p)
	}
	if len(p.Bindings) != 4 {
		t.Fatalf("bindings %+v", p.Bindings)
	}
	for _, b := range p.Bindings {
		found := false
		for _, o := range built.Doc.Shape.Objects {
			if o.Cluster == b.Object.Cluster && o.Kind == b.Object.Kind && o.Namespace == b.Object.Namespace && o.Name == b.Object.Name {
				found = true
				if o.Hash != b.ManifestHash {
					t.Fatal("binding differs from SAME ShapeObject.Hash")
				}
			}
		}
		if !found {
			t.Fatal("binding missing from sealed inventory")
		}
		if b.Object.Kind == "Job" && b.JobContract != release.KubernetesJobCompleteV1 {
			t.Fatal("missing ordinary Job completion contract")
		}
	}
	again, err := Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if built.Digest != again.Digest {
		t.Fatal("nondeterministic graph")
	}
}

func TestPlacementPrerequisitesUnitAfter(t *testing.T) {
	in := placementBuildInput()
	// Same-stage controller dependency through unit.after is retained.
	in.Shape.Manifests = strings.Replace(in.Shape.Manifests, "    forge.dev/workload: api", "    forge.dev/workload: api\n    forge.dev/delivery-unit: server", 1)
	in.Shape.Manifests += `---
# cluster: prod
apiVersion: apps/v1
kind: Deployment
metadata:
  name: worker
  namespace: app
  labels:
    forge.dev/workload: worker
    forge.dev/delivery-unit: worker
spec: {}
`
	in.Placements = append(in.Placements, PlacementDeclaration{Workload: "worker", Runtime: "cluster", Actuator: "forge"})
	in.Units = []release.DeliveryUnit{{Name: "server", After: []string{}}, {Name: "worker", After: []string{"server"}}}
	b, err := Build(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, edge := range b.Doc.PlacementPrerequisites.Edges {
		if edge.Predecessor.Name == "api" && edge.Dependent.Name == "worker" {
			found = true
		}
	}
	if !found {
		t.Fatal("same-stage unit.after edge dropped")
	}
}

func TestPlacementPrerequisitesRefusals(t *testing.T) {
	tests := map[string]struct {
		mutate func(*BuildInput)
		want   string
	}{
		"old reader":           {func(in *BuildInput) { in.Reader = &release.BundleReader{} }, release.BundleFeaturePlacementPrerequisites},
		"unknown declarations": {func(in *BuildInput) { in.Placements = nil }, "complete placement"},
		"missing peer":         {func(in *BuildInput) { in.Placements[1].Dependencies = []string{"missing"} }, "missing predecessor"},
		"hub":                  {func(in *BuildInput) { in.Placements[0].Actuator = "hub" }, "unsupported mixed"},
		"hosted to forge": {func(in *BuildInput) {
			in.Placements[1].Dependencies = nil
			in.Placements[0].Dependencies = []string{"web"}
		}, "hosted predecessor"},
		"cycle":             {func(in *BuildInput) { in.Placements[0].Dependencies = []string{"web"} }, "cycle"},
		"unit missing peer": {func(in *BuildInput) { in.Units = []release.DeliveryUnit{{Name: "default", After: []string{"missing"}}} }, "missing predecessor unit"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			in := placementBuildInput()
			tc.mutate(&in)
			_, err := Build(t.Context(), in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPlacementPrerequisitesSnapshotDependenciesStayFrozen(t *testing.T) {
	declarations := []PlacementDeclaration{{Workload: "api", Runtime: "cluster", Actuator: "forge"}, {Workload: "web", Runtime: "hosted", Actuator: "platform", Dependencies: []string{}}}
	s := release.Snapshot{Consumers: []release.Consumer{{Key: release.ConsumerKey{Cluster: "prod", Workload: "api"}, Slots: []string{"container:api"}, DependenciesDeclared: true}, {Key: release.ConsumerKey{Cluster: "frontend", Workload: "web"}, Slots: []string{"frontend"}, DependenciesDeclared: false}}}
	got, err := snapshotPlacements(s, declarations)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[1].Dependencies) != 1 || got[1].Dependencies[0] != "api" {
		t.Fatalf("checkout independence replaced frozen default: %+v", got)
	}
	s.Consumers[1].DependenciesDeclared = true
	declarations[1].Dependencies = []string{"api"}
	got, err = snapshotPlacements(s, declarations)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[1].Dependencies) != 0 {
		t.Fatalf("checkout dependency replaced frozen independence: %+v", got)
	}
}

func TestPlacementPrerequisitesIndependentAndPure(t *testing.T) {
	in := placementBuildInput()
	in.Placements[1].Dependencies = []string{}
	b, err := Build(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if b.Doc.PlacementPrerequisites == nil || len(b.Doc.PlacementPrerequisites.Edges) != 0 {
		t.Fatal("explicit independent graph lost")
	}
	// Pure historical input gets no new wire field, even with an old reader.
	in.Shape.Manifests = strings.Split(in.Shape.Manifests, "---\n# cluster: hosted")[0]
	in.Placements = nil
	in.Reader = &release.BundleReader{}
	b, err = Build(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if b.Doc.PlacementPrerequisites != nil || strings.Contains(string(b.ConfigBlob()), "placement_prerequisites") {
		t.Fatal("pure bundle changed")
	}
}
