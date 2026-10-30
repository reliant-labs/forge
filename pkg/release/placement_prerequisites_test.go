package release

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func placementFixture() *PlacementPrerequisites {
	a := PlacementKey{Runtime: "cluster", Actuator: "forge", Cluster: "prod", Namespace: "app", Kind: "Deployment", Name: "api"}
	b := PlacementKey{Runtime: "hosted", Actuator: "platform", Cluster: "hosted", Namespace: "app", Kind: "StaticSite", Name: "web"}
	return &PlacementPrerequisites{Version: PlacementPrerequisitesVersion, Items: []PlacementItem{{a, "api"}, {b, "web"}}, Edges: []PlacementEdge{{a, b}}, Bindings: []PlacementBinding{{Item: a, Object: PlacementObject{Cluster: "prod", APIVersion: "apps/v1", Kind: "Deployment", Namespace: "app", Name: "api"}, ManifestHash: "sha256:" + strings.Repeat("a", 64)}}}
}

func TestPlacementPrerequisitesValidation(t *testing.T) {
	tests := map[string]func(*PlacementPrerequisites){
		"version":             func(p *PlacementPrerequisites) { p.Version = 1 }, // retired with placement-prerequisites-v1
		"runtime":             func(p *PlacementPrerequisites) { p.Items[0].Key.Runtime = "unknown" },
		"actuator":            func(p *PlacementPrerequisites) { p.Items[0].Key.Actuator = "hub" },
		"namespace traversal": func(p *PlacementPrerequisites) { p.Items[0].Key.Namespace = "../app" },
		"cluster traversal":   func(p *PlacementPrerequisites) { p.Items[0].Key.Cluster = "../prod" },
		"duplicate":           func(p *PlacementPrerequisites) { p.Items = append(p.Items, p.Items[0]) },
		"reserved cluster":    func(p *PlacementPrerequisites) { p.Items[0].Key.Cluster = BundleHostedCluster },
		"namespace dot":       func(p *PlacementPrerequisites) { p.Items[0].Key.Namespace = "bad.ns" },
		"missing target":      func(p *PlacementPrerequisites) { p.Edges[0].Predecessor.Name = "absent" },
		"self":                func(p *PlacementPrerequisites) { p.Edges[0].Predecessor = p.Edges[0].Dependent },
		"duplicate edge":      func(p *PlacementPrerequisites) { p.Edges = append(p.Edges, p.Edges[0]) },
		"cycle": func(p *PlacementPrerequisites) {
			p.Edges = append(p.Edges, PlacementEdge{p.Items[1].Key, p.Items[0].Key})
		},
		"binding item":      func(p *PlacementPrerequisites) { p.Bindings[0].Item.Name = "absent" },
		"binding cluster":   func(p *PlacementPrerequisites) { p.Bindings[0].Object.Cluster = "foreign" },
		"binding namespace": func(p *PlacementPrerequisites) { p.Bindings[0].Object.Namespace = "foreign" },
		"binding hash":      func(p *PlacementPrerequisites) { p.Bindings[0].ManifestHash = "unknown" },
		"binding duplicate": func(p *PlacementPrerequisites) { p.Bindings = append(p.Bindings, p.Bindings[0]) },
		"job contract": func(p *PlacementPrerequisites) {
			p.Bindings[0].Object.Kind = "Job"
			p.Bindings[0].Object.APIVersion = "batch/v1"
		},
		"nonjob contract": func(p *PlacementPrerequisites) { p.Bindings[0].JobContract = KubernetesJobCompleteV1 },
		"secret":          func(p *PlacementPrerequisites) { p.Bindings[0].Object.Kind = "Secret" },
		"passive runtime": func(p *PlacementPrerequisites) { p.Passive[0].Kind = "Deployment" },
		"passive configmap": func(p *PlacementPrerequisites) {
			p.Passive[0].Kind, p.Passive[0].APIVersion = "ConfigMap", "v1"
		},
		"passive builtin":   func(p *PlacementPrerequisites) { p.Passive[0].Kind = "Service" },
		"passive hosted":    func(p *PlacementPrerequisites) { p.Passive[0].Cluster = BundleHostedCluster },
		"passive duplicate": func(p *PlacementPrerequisites) { p.Passive = append(p.Passive, p.Passive[0]) },
		"passive bound": func(p *PlacementPrerequisites) {
			p.Passive[0] = p.Bindings[0].Object
		},
		"passive name": func(p *PlacementPrerequisites) { p.Passive[0].Name = "Bad_Name" },
	}
	passiveFixture := func() *PlacementPrerequisites {
		p := placementFixture()
		p.Passive = []PlacementObject{{Cluster: "prod", APIVersion: "reliant.dev/v1alpha1", Kind: "DaemonRollout", Namespace: "app", Name: "daemon-rollout"}}
		return p
	}
	if err := passiveFixture().Validate(); err != nil {
		t.Fatalf("a declared-passive custom resource: %v", err)
	}
	if err := placementFixture().Validate(); err != nil {
		t.Fatal(err)
	}
	hosted := placementFixture()
	hosted.Edges = nil
	hosted.Items[1].Key.Namespace = ""
	if err := hosted.Validate(); err != nil {
		t.Fatalf("sealed hosted tree has no invented namespace: %v", err)
	}
	if err := (&PlacementPrerequisites{Version: PlacementPrerequisitesVersion}).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := passiveFixture()
			mutate(p)
			if err := p.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestPlacementPrerequisitesWireAndHistoricalBytes(t *testing.T) {
	d := fullBundleDoc()
	before, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "placement_prerequisites") {
		t.Fatal("historical field emitted")
	}
	decoded, err := DecodeBundleDoc(before)
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("historical bytes changed")
	}
	d.PlacementPrerequisites = placementFixture()
	// A safety contract is never silently removed even by a legacy limiter.
	limited := (&BundleReader{}).Limit(d)
	if limited.PlacementPrerequisites != d.PlacementPrerequisites {
		t.Fatal("safety contract discarded")
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = DecodeBundleDoc(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.PlacementPrerequisites == nil {
		t.Fatal("wire graph lost")
	}
	graphStart := `"placement_prerequisites":{"version":2`
	if !strings.Contains(string(raw), graphStart) {
		t.Fatalf("wire graph is not version 2: %s", raw)
	}
	raw = []byte(strings.Replace(string(raw), graphStart, graphStart+`,"unknown":true`, 1))
	if _, err := DecodeBundleDoc(raw); err == nil {
		t.Fatal("unknown graph field accepted")
	}
}
