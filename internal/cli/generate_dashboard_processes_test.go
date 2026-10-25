package cli

import (
	"reflect"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	deployv1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

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
