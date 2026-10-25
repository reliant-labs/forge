package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	deployv1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

func TestDashboardProcessesStructuralLibraryNeedsNoBoundEnvironment(t *testing.T) {
	if testing.Short() {
		t.Skip("evaluates KCL library; full package suite exercises it")
	}
	dir := t.TempDir()
	files := map[string]string{
		"forge.yaml":         "name: sample\nmodule_path: example.com/sample\n",
		"deploy/kcl/kcl.mod": "[package]\nname = \"test\"\nversion = \"0.0.1\"\n",
		"deploy/kcl/workloads.k": `import forge
import forge.workloads as fw
_name = "sample" + "-worker"
ALL = [fw.Workload {name = _name, kind = "worker", build = forge.GoBuild {cmd = "./cmd/sample"}, args = ["server", "worker"], env = {OTEL_SERVICE_NAME = "sample-jobs"}}]
`,
		"deploy/kcl/prod/main.k": "assert False, \"operator needs a cluster binding\"\n",
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := dashboardDeclaredWorkloads(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := processesForWorkloads("sample", []codegen.GraphRoot{{Name: "worker", Args: []string{"server", "worker"}}}, got)
	if err != nil || len(p) != 1 || p[0].Name != "sample-worker" || !reflect.DeepEqual(p[0].ServiceNames, []string{"sample-jobs"}) {
		t.Fatalf("structural inventory=%+v err=%v", p, err)
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
