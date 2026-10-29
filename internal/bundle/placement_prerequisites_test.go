package bundle

import (
	"context"
	"slices"
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

// unitDoc is one cluster object in a named delivery unit.
func unitDoc(kind, apiVersion, name, workload, unit string) string {
	labels := "    forge.dev/delivery-unit: " + unit + "\n"
	if workload != "" {
		labels += "    forge.dev/workload: " + workload + "\n"
	}
	return "---\n# cluster: prod\napiVersion: " + apiVersion + "\nkind: " + kind + "\nmetadata:\n  name: " + name +
		"\n  namespace: app\n  labels:\n" + labels + "spec: {}\n"
}

const hostedWebDoc = "---\n# cluster: hosted\napiVersion: forge.dev/v1alpha1\nkind: StaticSite\nmetadata:\n  name: web\nspec: {}\n"

// Delivery-unit order follows release.PlanTrees: a named unit with no
// declared `after` waits for base only when base renders. With no base
// objects at all the bundle builds, as a pure cluster env does.
func TestPlacementPrerequisitesNoBaseUnit(t *testing.T) {
	in := placementBuildInput()
	in.Shape.Manifests = unitDoc("Deployment", "apps/v1", "api", "api", "server") + hostedWebDoc
	in.Units = []release.DeliveryUnit{{Name: "server"}}
	b, err := Build(t.Context(), in)
	if err != nil {
		t.Fatalf("mixed env without base objects: %v", err)
	}
	p := b.Doc.PlacementPrerequisites
	if len(p.Edges) != 1 || p.Edges[0].Predecessor.Name != "api" || p.Edges[0].Dependent.Name != "web" {
		t.Fatalf("edges = %+v, want only api -> web", p.Edges)
	}
}

// A unit holding only support objects is not an item, and ordering passes
// through it: db (controller) <- support (ConfigMap only) <- server.
// Base holds only a support object too.
func TestPlacementPrerequisitesSupportOnlyUnits(t *testing.T) {
	in := placementBuildInput()
	in.Placements = append(in.Placements, PlacementDeclaration{Workload: "db", Runtime: "cluster", Actuator: "forge"})
	in.Shape.Manifests = unitDoc("ServiceAccount", "v1", "base-sa", "", release.DefaultDeliveryUnit) +
		unitDoc("Deployment", "apps/v1", "db", "db", "data") +
		unitDoc("ConfigMap", "v1", "settings", "", "support") +
		unitDoc("Deployment", "apps/v1", "api", "api", "server") + hostedWebDoc
	in.Units = []release.DeliveryUnit{{Name: "data"}, {Name: "support", After: []string{"data"}}, {Name: "server", After: []string{"support"}}}
	b, err := Build(t.Context(), in)
	if err != nil {
		t.Fatalf("support-only units: %v", err)
	}
	want := map[string]bool{"db->api": true, "api->web": false}
	for _, e := range b.Doc.PlacementPrerequisites.Edges {
		key := e.Predecessor.Name + "->" + e.Dependent.Name
		if _, ok := want[key]; ok {
			want[key] = true
		}
		if e.Predecessor.Kind == "ConfigMap" || e.Dependent.Kind == "ConfigMap" || e.Predecessor.Kind == "ServiceAccount" {
			t.Fatalf("support object became an item: %+v", e)
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("edge %s missing from %+v", key, b.Doc.PlacementPrerequisites.Edges)
		}
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
		"cycle": {func(in *BuildInput) { in.Placements[0].Dependencies = []string{"web"} }, "cycle"},
		"unit missing peer": {func(in *BuildInput) {
			in.Units = []release.DeliveryUnit{{Name: release.DefaultDeliveryUnit, After: []string{"missing"}}}
		}, "which nothing declares or renders"},
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

// A build-only consumer is an artifact, not a placement: a sealed mixed
// composition carrying one projects only its runtime consumers.
func TestPlacementPrerequisitesSnapshotSkipsBuildOnly(t *testing.T) {
	declarations := []PlacementDeclaration{{Workload: "api", Runtime: "cluster", Actuator: "forge"}, {Workload: "web", Runtime: "hosted", Actuator: "platform"}}
	s := release.Snapshot{Consumers: []release.Consumer{
		{Key: release.ConsumerKey{Cluster: "prod", Workload: "api"}, Slots: []string{"container:api"}, DependenciesDeclared: true},
		{Key: release.ConsumerKey{Cluster: "build-only", Workload: "cli"}, Slots: []string{"build-only"}, DependenciesDeclared: true},
		{Key: release.ConsumerKey{Cluster: "frontend", Workload: "web"}, Slots: []string{"frontend"}},
	}}
	got, err := snapshotPlacements(s, declarations)
	if err != nil {
		t.Fatalf("build-only consumer blocked sealing: %v", err)
	}
	if len(got) != 2 || got[1].Workload != "web" || !slices.Equal(got[1].Dependencies, []string{"api"}) {
		t.Fatalf("placements = %+v, want api and web (after api only)", got)
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
