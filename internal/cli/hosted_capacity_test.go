package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
	deployv1alpha1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

type capacityFake struct {
	capacity map[string]any
	capErr   error
	calls    []string
}

func (f *capacityFake) Call(_ context.Context, proc string, _, out any) error {
	f.calls = append(f.calls, proc)
	var reply any
	switch proc {
	case "controlplane.v1.DeployService/ListEnvironments":
		reply = map[string]any{"environments": []any{map[string]any{"id": "env-1", "name": "prod", "project": ""}}}
	case "controlplane.v1.DeployService/CheckDeployCapacity":
		if f.capErr != nil {
			return f.capErr
		}
		reply = f.capacity
	default:
		return errors.New("a side-effecting RPC was called before the capacity pre-flight finished: " + proc)
	}
	return decodeFake(reply, out)
}

type wireCoded struct{ code string }

func (e wireCoded) Error() string         { return "wire " + e.code }
func (e wireCoded) HasCode(c string) bool { return c == e.code }

func houndersEntities() *KCLEntities {
	job := hostedWL("migrate", func(w *WorkloadEntity) {
		w.Kind = "job"
		w.Spec.Kind = deployv1alpha1.KindJob
		w.Spec.Ports, w.Spec.Probes = nil, nil
		w.Spec.Args = []string{"db", "migrate", "up"}
	})
	return &KCLEntities{
		ControlPlane: &ControlPlaneEntity{Type: "control_plane", Endpoint: "https://cp.example"},
		Workloads:    []WorkloadEntity{hostedWL("api"), job},
		Databases:    []DatabaseEntity{{Name: "db", Runtime: RuntimeHosted, Spec: deployv1alpha1.ManagedDatabaseSpec{StorageGiB: 1}}},
		Frontends:    []FrontendEntity{{Name: "web", Image: "ghcr.io/acme/web", Runtime: FrontendRuntime{Type: FrontendRuntimeHosted}}},
	}
}

func withCapacityFake(t *testing.T, f *capacityFake) {
	t.Helper()
	t.Setenv(cloud.DefaultTokenEnv, "rlat_test")
	withProjectName(t, "hounders")
	prev := hostedDeployClient
	hostedDeployClient = func(cloud.Endpoint, cloud.Credential) deploytarget.HostedCaller { return f }
	t.Cleanup(func() { hostedDeployClient = prev })
}

func TestHostedCapacityPreflight_RefusedBlocksWithReasonAndFix(t *testing.T) {
	f := &capacityFake{capacity: map[string]any{
		"allowed": false, "code": "DEPLOY_CAPACITY_CODE_NO_COMPUTE_PLAN",
		"reason": "this runs compute and the organization has no active compute plan",
		"fix":    "subscribe to a Reliant Compute plan in billing settings, then retry",
	}}
	withCapacityFake(t, f)
	var out strings.Builder
	err := hostedCapacityPreflight(context.Background(), "prod", houndersEntities(), 1, &out)
	var refused *deploytarget.CapacityRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want *CapacityRefusedError", err)
	}
	for _, s := range []string{"no active compute plan", "subscribe to a Reliant Compute plan", "2 workload(s), 1 database(s), 1 build(s), 1 static site(s)"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error missing %q:\n%s", s, err)
		}
	}
	for _, c := range f.calls {
		if !strings.HasSuffix(c, "ListEnvironments") && !strings.HasSuffix(c, "CheckDeployCapacity") {
			t.Errorf("unexpected side-effecting call %s", c)
		}
	}
}

func TestHostedCapacityPreflight_AllowedProceeds(t *testing.T) {
	withCapacityFake(t, &capacityFake{capacity: map[string]any{"allowed": true, "code": "DEPLOY_CAPACITY_CODE_OK", "hasComputePlan": true,
		"ceilingCpuMillicores": "8000", "ceilingMemoryBytes": "17179869184", "ceilingStorageGib": "100"}})
	var out strings.Builder
	if err := hostedCapacityPreflight(context.Background(), "prod", houndersEntities(), 1, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Capacity: OK") {
		t.Errorf("output = %q", out.String())
	}
}

func TestHostedCapacityPreflight_UnimplementedWarnsAndProceeds(t *testing.T) {
	withCapacityFake(t, &capacityFake{capErr: wireCoded{"unimplemented"}})
	var out strings.Builder
	if err := hostedCapacityPreflight(context.Background(), "prod", houndersEntities(), 1, &out); err != nil {
		t.Fatalf("unimplemented must continue, got %v", err)
	}
	if !strings.Contains(out.String(), "WARNING: capacity could not be pre-checked") {
		t.Errorf("output = %q", out.String())
	}
}

func TestHostedCapacityPreflight_NetworkErrorFails(t *testing.T) {
	withCapacityFake(t, &capacityFake{capErr: wireCoded{"unavailable"}})
	if err := hostedCapacityPreflight(context.Background(), "prod", houndersEntities(), 1, &strings.Builder{}); err == nil {
		t.Fatal("a failed check must fail the deploy, not fall open")
	}
}

func TestHostedCapacityPreflight_PlanUnknownFailsWithRetry(t *testing.T) {
	withCapacityFake(t, &capacityFake{capacity: map[string]any{"allowed": false, "code": "DEPLOY_CAPACITY_CODE_PLAN_UNKNOWN", "reason": "lookup failed", "fix": "retry"}})
	err := hostedCapacityPreflight(context.Background(), "prod", houndersEntities(), 0, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "retry") || !strings.Contains(err.Error(), "PLAN_UNKNOWN") {
		t.Fatalf("err = %v", err)
	}
}

// The release deploy must refuse before ensureHostedReleaseBundle / promote:
// any other RPC reaching the fake fails the test.
func TestDispatchReleaseDeploy_RefusedBeforeAnySideEffect(t *testing.T) {
	f := &capacityFake{capacity: map[string]any{"allowed": false, "code": "DEPLOY_CAPACITY_CODE_NO_COMPUTE_PLAN", "reason": "no plan", "fix": "subscribe"}}
	withCapacityFake(t, f)
	dir := t.TempDir()
	fixture := filepath.Join(dir, "render.json")
	if err := os.WriteFile(fixture, contractJSON(t, houndersEntities()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", fixture)
	err := hostedCapacityPreflightForEnv(context.Background(), dir, "prod", false, &strings.Builder{})
	var refused *deploytarget.CapacityRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != 2 {
		t.Errorf("calls = %v, want only the lookup and the check", f.calls)
	}
}

func decodeFake(reply any, out any) error {
	b, _ := json.Marshal(reply)
	return json.Unmarshal(b, out)
}
