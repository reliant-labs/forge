package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/obsdash"
	deployv1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

func TestDashboardProcessesSelectedRenderNeedsNoOtherEnvironment(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"forge.yaml":             "name: sample\nmodule_path: example.com/sample\n",
		"go.mod":                 "module example.com/sample\n\ngo 1.25\n",
		"deploy/kcl/prod/main.k": "assert False, \"operator needs a cluster binding\"\n",
		"deploy/kcl/workloads.k": "ALL = []\n",
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := obsdash.Write(dir, obsdash.Input{Project: "sample"}, &checksums.FileChecksums{}); err != nil {
		t.Fatal(err)
	}
	// The selected render has an env-only _api absent from source ALL, with an
	// env-specific telemetry alias. Neither ALL nor prod is evaluated.
	entities := &KCLEntities{Workloads: []WorkloadEntity{{Name: "dev-api", Build: BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/sample"}}, Spec: deployv1.WorkloadSpec{Args: []string{"server"}, Env: []deployv1.EnvVar{{Name: "OTEL_SERVICE_NAME", Value: "dev-telemetry"}}}}}}
	sp, err := dashboardSpecForEntities(dir, entities)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range sp.Dashboards {
		if d.ID == "process-sample-server" {
			found = true
			for _, p := range d.Panels {
				if p.ID == "go-memory" && !reflect.DeepEqual(p.Where[0].Values, []string{"dev-telemetry"}) {
					t.Fatalf("lost rendered alias: %+v", p.Where)
				}
			}
		}
	}
	if !found {
		t.Fatal("environment-only process missing")
	}
	if _, err := dashboardSpecForEntities(dir, nil); err == nil {
		t.Fatal("missing selected render accepted")
	}
}

func TestDashboardProcessesUseExecutableAndCompositionCommand(t *testing.T) {
	roots := []codegen.GraphRoot{
		{Name: "api", Args: []string{"server", "api"}, RPCServices: []string{"reliant.v1.ChatService"}, HasDatabase: true},
		{Name: "worker", Args: []string{"server", "worker"}},
		{Name: "gateway", Args: []string{"server", "gateway"}, RPCServices: []string{"reliant.v1.DaemonService"}},
	}
	workload := func(name, bin string, args ...string) WorkloadEntity {
		return WorkloadEntity{Name: name, Kind: "service", Build: BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/" + bin}}, Spec: deployv1.WorkloadSpec{Args: args}, Runtime: RuntimeEntity{Type: RuntimeHosted}}
	}
	api := workload("api", "reliant", "server", "api")
	apiAlias := workload("renamed-api", "reliant", "server", "api")
	apiAlias.Spec.Env = []deployv1.EnvVar{{Name: "OTEL_SERVICE_NAME", Value: "api-telemetry"}}
	worker := workload("worker", "reliant", "server", "worker")
	gateway := workload("gateway", "reliant", "server", "gateway")
	unrelated := workload("other", "other", "server", "api")
	buildOnly := workload("image", "reliant", "server", "api")
	buildOnly.Runtime.Type = RuntimeBuildOnly
	got, err := processesForWorkloads("reliant", roots, []WorkloadEntity{worker, apiAlias, gateway, api, unrelated, api, buildOnly})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("processes=%+v", got)
	}
	if got[0].Name != "other-server-api" || len(got[0].RPCServices) != 0 {
		t.Fatalf("secondary binary inherited primary root: %+v", got[0])
	}
	if got[1].Name != "reliant-api" || !reflect.DeepEqual(got[1].ServiceNames, []string{"api", "api-telemetry"}) || len(got[1].Workloads) != 2 || !got[1].HasDatabase {
		t.Fatalf("api aliases=%+v", got[1])
	}
	if got[2].Root != "gateway" || len(got[2].RPCServices) != 1 {
		t.Fatalf("gateway=%+v", got[2])
	}
	if got[3].Root != "worker" || len(got[3].RPCServices) != 0 {
		t.Fatalf("worker=%+v", got[3])
	}
}
