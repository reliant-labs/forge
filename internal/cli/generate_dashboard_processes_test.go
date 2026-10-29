package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
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
		if d.ID == "process-dev-api" {
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
	// Named for the forge workload (the first alphabetically when several run
	// the same executable and command), never the command line.
	names := map[string]obsdash.Process{}
	for _, p := range got {
		names[p.Name] = p
	}
	if len(got) != 4 || names["api"].Binary == "" || names["gateway"].Binary == "" || names["worker"].Binary == "" || names["other"].Binary == "" {
		t.Fatalf("processes=%+v", got)
	}
	if p := names["other"]; len(p.RPCServices) != 0 {
		t.Fatalf("secondary binary inherited primary root: %+v", p)
	}
	if p := names["api"]; !reflect.DeepEqual(p.ServiceNames, []string{"api", "api-telemetry"}) || !reflect.DeepEqual(p.Workloads, []string{"api", "renamed-api"}) || !p.HasDatabase || p.Root != "api" {
		t.Fatalf("api aliases=%+v", p)
	}
	if got := names["api"].Replaces; !reflect.DeepEqual(got, []string{"reliant-api"}) {
		t.Fatalf("api must remember the id it was generated under: %v", got)
	}
	if p := names["gateway"]; p.Root != "gateway" || len(p.RPCServices) != 1 {
		t.Fatalf("gateway=%+v", p)
	}
	if p := names["worker"]; p.Root != "worker" || len(p.RPCServices) != 0 {
		t.Fatalf("worker=%+v", p)
	}
}

// forge generate scopes the overview to every environment's deployed
// identities: each environment's aliases (OTEL_SERVICE_NAME, or the workload
// name) count, a build-only workload does not, and the result is the same
// whatever order the environments rendered in.
func TestDeployedIdentitiesSpanEveryEnvironment(t *testing.T) {
	dir := t.TempDir()
	workload := func(name string, env ...deployv1.EnvVar) WorkloadEntity {
		return WorkloadEntity{Name: name, Kind: "service", Build: BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/app"}}, Spec: deployv1.WorkloadSpec{Args: []string{"server"}, Env: env}}
	}
	dev := workload("app-dev")
	prod := workload("app", deployv1.EnvVar{Name: "OTEL_SERVICE_NAME", Value: "app-api"})
	image := workload("app-image")
	image.Runtime.Type = RuntimeBuildOnly
	ctx := &pipelineContext{AbsPath: dir, ProjectDir: dir, Cfg: &config.ProjectConfig{Name: "app"}, graphRootsSet: true}
	got, err := deployedIdentities(ctx, []WorkloadEntity{prod, image, dev, prod})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"app-api", "app-dev"}; !reflect.DeepEqual(got.ServiceNames, want) {
		t.Errorf("ServiceNames = %v, want %v", got.ServiceNames, want)
	}
	if want := []string{"app", "app-dev"}; !reflect.DeepEqual(got.Workloads, want) {
		t.Errorf("Workloads = %v, want %v", got.Workloads, want)
	}
	if got, err := deployedIdentities(ctx, nil); err != nil || len(got.ServiceNames)+len(got.Workloads) != 0 {
		t.Errorf("no workloads = %+v, %v; want no identities", got, err)
	}
}

func wl(name, kind string, args ...string) WorkloadEntity {
	return WorkloadEntity{Name: name, Kind: kind, Build: BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/app"}},
		Spec: deployv1.WorkloadSpec{Kind: deployv1.WorkloadKind(kind), Args: args}, Runtime: RuntimeEntity{Type: RuntimeCluster}}
}

// A rendered Job is "<name>-<hash>" and a hand-built argument list can carry a
// URL: neither may reach a dashboard name. The name is the forge workload's.
func TestProcessNamesComeFromTheForgeWorkloadNotItsArgumentsOrHash(t *testing.T) {
	canary := wl("identity-canary", "worker", "identity-canary", "--admin-api-url", "http://admin-api.control-plane-prod.svc.cluster.local:80")
	prune := wl("cloudsql-backup-prune", "cron", "cloudsql", "prune-backups", "--project", "reliant-labs-475814", "--instance", "reliant-pg", "--older-than", "720h")
	migrate := wl("control-plane-migrate", "job", "db", "migrate", "up")
	got, err := processesForWorkloads("app", nil, []WorkloadEntity{canary, prune, migrate})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
		for _, bad := range []string{"---", "http", ".svc.", "--project"} {
			if strings.Contains(p.Name, bad) {
				t.Errorf("process name %q carries rendered arguments (%q)", p.Name, bad)
			}
		}
	}
	if want := []string{"cloudsql-backup-prune", "control-plane-migrate", "identity-canary"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	// Every old id is remembered so a star or copy of it can follow.
	for _, p := range got {
		if len(p.Replaces) != 1 || !strings.HasPrefix(p.Replaces[0], "app-") {
			t.Errorf("%s replaces %v, want the one id it was generated under", p.Name, p.Replaces)
		}
	}
}

func TestProcessKindsLimitsAndRuntimes(t *testing.T) {
	limited := wl("api", "service", "api")
	limited.Spec.Resources = deployv1.Resources{CPURequestMillicores: 100, CPULimitMillicores: 500, MemoryRequestBytes: 1 << 27}
	requestOnly := wl("worker", "worker", "worker")
	requestOnly.Spec.Resources = deployv1.Resources{CPURequestMillicores: 100, MemoryRequestBytes: 1 << 27}
	host := wl("hostapi", "service", "host")
	host.Runtime = RuntimeEntity{Type: RuntimeHost}
	tool := wl("tool", "tool", "tool")
	got, err := processesForWorkloads("app", nil, []WorkloadEntity{limited, requestOnly, host, tool})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]obsdash.Process{}
	for _, p := range got {
		by[p.Name] = p
	}
	if _, ok := by["tool"]; ok {
		t.Error("a tool runs nothing and has no dashboard")
	}
	if p := by["api"]; !p.CPULimit || p.MemoryLimit || !p.NodeMetadata || p.Kind != "service" {
		t.Errorf("api: %+v (cpu limit declared, memory limit not)", p)
	}
	if p := by["worker"]; p.CPULimit || p.MemoryLimit || p.Kind != "worker" {
		t.Errorf("a request-only worker declares no limit: %+v", p)
	}
	if p := by["hostapi"]; p.NodeMetadata {
		t.Errorf("a host process has no node agent: %+v", p)
	}
}

// reliant-api-server, reliant-temporal-worker and daemon-gateway are built from
// a sibling repository: forge sees no executable, but they are deployed, so
// they are processes.
func TestSiblingBuiltWorkloadsAreProcesses(t *testing.T) {
	sib := func(name, kind string, ports ...int32) WorkloadEntity {
		w := WorkloadEntity{Name: name, Kind: kind, Build: BuildConfigEntity{Type: "shell", Shell: &ShellBuild{Cmd: "make"}},
			Spec: deployv1.WorkloadSpec{Kind: deployv1.WorkloadKind(kind)}, Runtime: RuntimeEntity{Type: RuntimeCluster}}
		for i, p := range ports {
			w.Spec.Ports = append(w.Spec.Ports, deployv1.Port{Name: fmt.Sprintf("p%d", i), Port: p})
		}
		return w
	}
	got, err := processesForWorkloads("control-plane", nil, []WorkloadEntity{
		sib("reliant-api-server", "service", 9090), sib("reliant-temporal-worker", "worker", 8095), sib("daemon-gateway", "service", 9190),
	})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]obsdash.Process{}
	for _, p := range got {
		by[p.Name] = p
	}
	for _, n := range []string{"reliant-api-server", "reliant-temporal-worker", "daemon-gateway"} {
		if _, ok := by[n]; !ok {
			t.Errorf("%s missing from the process inventory", n)
		}
	}
	if !by["reliant-api-server"].ServesRPC() || !by["daemon-gateway"].ServesRPC() {
		t.Error("a sibling-built service with a port serves RPCs")
	}
	if by["reliant-temporal-worker"].ServesRPC() {
		t.Error("a worker serves no RPCs")
	}
	if len(by["reliant-api-server"].Replaces) != 0 {
		t.Errorf("these never had a dashboard, so replace nothing: %v", by["reliant-api-server"].Replaces)
	}
	id, err := deployedIdentities(&pipelineContext{AbsPath: t.TempDir(), Cfg: &config.ProjectConfig{Name: "control-plane"}, graphRootsSet: true},
		[]WorkloadEntity{sib("reliant-api-server", "service", 9090), sib("reliant-temporal-worker", "worker", 8095)})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"reliant-api-server", "reliant-temporal-worker"}; !reflect.DeepEqual(id.ServiceNames, want) || !reflect.DeepEqual(id.RPCServiceNames, []string{"reliant-api-server"}) {
		t.Errorf("identities = %+v, want both in the overview scope and only the api in the RPC scope", id)
	}
}

// A third-party image or a Docker-built Next.js container is not a Go program:
// it has no Go runtime and serves no RPC metrics, however many ports it opens.
func TestNonGoImagesGetNeitherRPCNorGoRuntimePanels(t *testing.T) {
	registry := WorkloadEntity{Name: "registry", Kind: "service", Spec: deployv1.WorkloadSpec{Kind: deployv1.KindService, Ports: []deployv1.Port{{Name: "http", Port: 5000}}}, Runtime: RuntimeEntity{Type: RuntimeCluster}}
	console := WorkloadEntity{Name: "internal-console", Kind: "service", Build: BuildConfigEntity{Type: "docker", Docker: &DockerBuild{}},
		Spec: deployv1.WorkloadSpec{Kind: deployv1.KindService, Ports: []deployv1.Port{{Name: "http", Port: 3000}}}, Runtime: RuntimeEntity{Type: RuntimeCluster}}
	got, err := processesForWorkloads("control-plane", nil, []WorkloadEntity{registry, console})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if !p.NonGo || p.ServesRPC() {
			t.Errorf("%s: %+v", p.Name, p)
		}
	}
	sp := obsdash.Build(obsdash.Input{Project: "control-plane", Processes: got})
	for _, d := range sp.Dashboards {
		for _, p := range d.Panels {
			if strings.HasPrefix(p.ID, "go-") || p.ID == "red-by-rpc" {
				t.Errorf("%s has %s", d.ID, p.ID)
			}
		}
	}
}

// The frontend dashboard follows the frontends the environment serves.
func TestFrontendIdentitiesFollowWhatTheEnvironmentServes(t *testing.T) {
	names, services := frontendIdentities("control-plane", []FrontendEntity{
		{Name: "reliant-web", Type: "vite-spa", Source: &config.GitSource{Repo: "github.com/reliant-labs/reliant"}},
		{Name: "reliant-web", Type: "vite-spa", Source: &config.GitSource{Repo: "github.com/reliant-labs/reliant"}},
		{Name: "app", Type: "react-native"},
		{Name: "site", Type: "nextjs"},
	})
	if !reflect.DeepEqual(names, []string{"reliant-web", "site"}) {
		t.Errorf("names = %v", names)
	}
	// An in-repo frontend reports <project>-<name>; one built elsewhere also
	// reports under its own declared name.
	if want := []string{"control-plane-reliant-web", "reliant-web", "control-plane-site"}; !reflect.DeepEqual(services, want) {
		t.Errorf("services = %v, want %v", services, want)
	}
	n, s := frontendIdentities("p", nil)
	if len(n)+len(s) != 0 {
		t.Errorf("an env serving no frontend has none: %v %v", n, s)
	}
}

// An environment with no deployed process (a static site) gets no overview,
// workloads or releases dashboard: the overview would filter on the
// no-deployed-process sentinel and everything would read "No data".
func TestProcessLessEnvironmentGetsOnlyItsFrontendDashboard(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"forge.yaml": "name: sample\nmodule_path: example.com/sample\n",
		"go.mod":     "module example.com/sample\n\ngo 1.25\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := obsdash.Write(dir, obsdash.Input{Project: "sample", Frontends: []string{"web"}}, &checksums.FileChecksums{}); err != nil {
		t.Fatal(err)
	}
	site := &KCLEntities{Frontends: []FrontendEntity{{Name: "web", Type: "vite-spa", Runtime: FrontendRuntime{Type: FrontendRuntimeHosted}}}}
	sp, err := dashboardSpecForEntities(dir, site)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range sp.Dashboards {
		ids = append(ids, d.ID)
	}
	if !reflect.DeepEqual(ids, []string{"frontend"}) {
		t.Fatalf("a process-less env has dashboards %v, want only the frontend", ids)
	}
	b, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "sample-web") {
		t.Error("the frontend dashboard is scoped to the frontend the env serves")
	}
}
