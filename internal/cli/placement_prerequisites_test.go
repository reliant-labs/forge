package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

func TestPlacementPrerequisitesReaderFirst(t *testing.T) {
	e := &KCLEntities{Workloads: []WorkloadEntity{orderClusterWorkload("api", nil)}, Frontends: []FrontendEntity{orderFrontend("web", FrontendRuntimeHosted, nil)}}
	previous := bundleRecorderForEnv
	t.Cleanup(func() { bundleRecorderForEnv = previous })
	for _, supported := range []bool{false, true} {
		features := []string{}
		if supported {
			features = release.BundleDocFeatures()
		}
		fake := &fakeDSOTCaller{replies: map[string]any{procDeployCapabilities: map[string]any{"bundleDocumentFeatures": features}}}
		bundleRecorderForEnv = func(context.Context, string, string, envLedger) (bundleRecorder, error) {
			return hostedRecordStoreFor(fake, "p"), nil
		}
		err := requirePlacementReader(t.Context(), t.TempDir(), "prod", e, envLedger{Hosted: true})
		if supported && err != nil {
			t.Fatal(err)
		}
		if !supported && (err == nil || !strings.Contains(err.Error(), release.BundleFeaturePlacementPrerequisites)) {
			t.Fatalf("old reader accepted: %v", err)
		}
		for _, call := range fake.calls {
			if call.Proc != procDeployCapabilities {
				t.Fatalf("effect before compatibility: %v", fake.calls)
			}
		}
	}
}

func TestPlacementPrerequisitesFrontendDeclarations(t *testing.T) {
	e := &KCLEntities{Workloads: []WorkloadEntity{orderClusterWorkload("api", nil), orderClusterWorkload("worker", map[string]string{"api": "v1"})}, Frontends: []FrontendEntity{orderFrontend("web", FrontendRuntimeHosted, nil)}}
	decl, err := bundlePlacementDeclarations(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(decl) != 3 || len(decl[2].Dependencies) != 2 || len(decl[1].Dependencies) != 1 {
		t.Fatalf("missing conservative/same-stage edges: %+v", decl)
	}
	e.Frontends[0].ReleaseDependencies = map[string]string{}
	decl, err = bundlePlacementDeclarations(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(decl[2].Dependencies) != 0 {
		t.Fatal("explicit empty treated as absent")
	}
	e.Frontends[0].ReleaseDependencies = map[string]string{"absent": "v1"}
	if _, err := bundlePlacementDeclarations(e); err == nil || !strings.Contains(err.Error(), "missing predecessor") {
		t.Fatalf("missing peer discarded: %v", err)
	}
}

// The composition snapshot keeps a build-only artifact consumer; the
// placement declarations (the preflight, before any build) omit it, and every
// runtime consumer of the snapshot has a declaration, so sealing cannot fail.
func TestPlacementPrerequisitesCompositionWithBuildOnly(t *testing.T) {
	e := &KCLEntities{
		Workloads: []WorkloadEntity{
			{Name: "api", Runtime: RuntimeEntity{Type: RuntimeCluster, Cluster: &ClusterRuntime{Cluster: "prod"}}},
			{Name: "cli", Runtime: RuntimeEntity{Type: RuntimeBuildOnly}},
		},
		Frontends: []FrontendEntity{orderFrontend("web", FrontendRuntimeHosted, nil)},
	}
	consumers, err := compositionConsumers(e)
	if err != nil {
		t.Fatal(err)
	}
	decl, err := bundlePlacementDeclarations(e)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, d := range decl {
		declared[d.Workload] = true
	}
	if declared["cli"] || !declared["api"] || !declared["web"] {
		t.Fatalf("declarations = %+v", decl)
	}
	sawBuildOnly := false
	for _, c := range consumers {
		if c.Key.Workload == "cli" {
			sawBuildOnly = true
			continue
		}
		if !declared[c.Key.Workload] {
			t.Errorf("runtime consumer %s has no placement declaration", c.Key.Workload)
		}
	}
	if !sawBuildOnly {
		t.Fatal("build-only artifact dropped from the composition snapshot")
	}
}

// The native OpenBao provider is a composition consumer, so it is declared
// as an ordinary cluster placement under its release owner, and a frontend
// with absent release_dependencies waits for it like any server.
func TestPlacementPrerequisitesNativeOpenBao(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	e := &KCLEntities{
		Workloads: []WorkloadEntity{{Name: "api", Runtime: RuntimeEntity{Type: RuntimeCluster, Cluster: &ClusterRuntime{Cluster: "prod"}}}},
		Frontends: []FrontendEntity{orderFrontend("web", FrontendRuntimeHosted, nil)},
		OpenBao:   &OpenBaoEntity{Name: "store", Namespace: "secrets", Cluster: "prod", ReleaseOwner: "openbao-secrets-store", ReleaseInputs: map[string]string{"container:openbao": "registry.example/openbao@" + digest, "container:converge": "registry.example/converge@" + digest}},
	}
	consumers, err := compositionConsumers(e)
	if err != nil {
		t.Fatal(err)
	}
	decl, err := bundlePlacementDeclarations(e)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]int{}
	for i, d := range decl {
		byName[d.Workload] = i
	}
	for _, c := range consumers {
		if _, ok := byName[c.Key.Workload]; !ok {
			t.Errorf("composition consumer %s has no placement declaration", c.Key.Workload)
		}
	}
	bao := decl[byName["openbao-secrets-store"]]
	if bao.Runtime != "cluster" || bao.Actuator != "forge" {
		t.Errorf("OpenBao placement = %+v", bao)
	}
	if web := decl[byName["web"]]; !slices.Contains(web.Dependencies, "openbao-secrets-store") || !slices.Contains(web.Dependencies, "api") {
		t.Errorf("absent frontend dependencies must include every server, OpenBao too: %+v", web)
	}
	e.OpenBao.ReleaseOwner = ""
	if _, err := bundlePlacementDeclarations(e); err == nil {
		t.Error("an OpenBao with no release owner cannot be placed")
	}
}

func TestPlacementPrerequisitesSameStageCycle(t *testing.T) {
	e := &KCLEntities{Workloads: []WorkloadEntity{orderClusterWorkload("a", map[string]string{"b": "v1"}), orderClusterWorkload("b", map[string]string{"a": "v1"})}}
	if _, err := deployStageOrder(e); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("same-stage cycle ignored: %v", err)
	}
}

func TestPlacementPrerequisitesFalseStageCycle(t *testing.T) {
	e := &KCLEntities{Workloads: []WorkloadEntity{orderHostedWorkload("a", nil), orderClusterWorkload("b", map[string]string{"a": "v1"}), orderClusterWorkload("c", nil), orderHostedWorkload("d", map[string]string{"c": "v1"})}}
	if _, err := declaredPlacementEdges(e); err != nil {
		t.Fatal(err)
	}
	if _, err := deployStageOrder(e); err == nil || !strings.Contains(err.Error(), "unsupported placement stage interleaving") || strings.Contains(err.Error(), "in a cycle") {
		t.Fatalf("false cycle interpretation: %v", err)
	}
}

func TestPlacementPrerequisitesUnsupportedAndPure(t *testing.T) {
	e := &KCLEntities{Workloads: []WorkloadEntity{orderClusterWorkload("api", nil)}, Frontends: []FrontendEntity{orderFrontend("web", FrontendRuntimeHosted, nil)}, ControlPlane: &ControlPlaneEntity{}, HubConverges: []string{release.DefaultDeliveryUnit}}
	// Explicit actuator rejection is also enforced by bundle Build; declaration
	// must preserve the actuator instead of guessing forge from runtime alone.
	decl, err := bundlePlacementDeclarations(e)
	if err == nil && decl[0].Actuator != "hub" {
		t.Fatalf("hub identity lost: %+v", decl)
	}
	e.Frontends = nil
	e.ControlPlane = nil
	e.HubConverges = nil
	decl, err = bundlePlacementDeclarations(e)
	if err != nil || decl != nil {
		t.Fatalf("pure deployment changed: %+v, %v", decl, err)
	}
}
