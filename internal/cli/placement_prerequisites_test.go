package cli

import (
	"context"
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
