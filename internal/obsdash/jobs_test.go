package obsdash

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func panelIDs(d Dashboard) []string {
	var ids []string
	for _, p := range d.Panels {
		ids = append(ids, p.ID)
	}
	return ids
}

func hasPanel(d Dashboard, id string) bool { _, ok := findPanel(d, id); return ok }

func dashboardByID(t *testing.T, sp Spec, id string) Dashboard {
	t.Helper()
	for _, d := range sp.Dashboards {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("no dashboard %q in %v", id, dashboardIDs(sp))
	return Dashboard{}
}

func dashboardIDs(sp Spec) []string {
	var ids []string
	for _, d := range sp.Dashboards {
		ids = append(ids, d.ID)
	}
	return ids
}

func usesMetric(d Dashboard, metric string) bool {
	for _, p := range d.Panels {
		for _, s := range p.Series {
			if s.Field == metric {
				return true
			}
		}
	}
	return false
}

// fleet is a project with one process of every kind, as control-plane's prod
// env declares them.
func fleet() Input {
	proc := func(name, kind string, mod func(*Process)) Process {
		p := Process{Name: name, Kind: kind, Binary: "app", Args: []string{name}, ServiceNames: []string{name}, Workloads: []string{name}, NodeMetadata: true}
		if mod != nil {
			mod(&p)
		}
		return p
	}
	return Input{
		Project: "shop",
		Processes: []Process{
			proc("admin-api", "service", func(p *Process) {
				p.RPCServices = []string{"orders.v1.OrdersService"}
				p.HasDatabase, p.CPULimit, p.MemoryLimit = true, true, true
			}),
			proc("workers", "worker", func(p *Process) { p.CPULimit = true }),
			proc("workspace", "operator", nil),
			proc("db-migrate-up", "job", nil),
			proc("dbrole-provision-reliant", "job", nil),
			proc("cloudsql-backup-prune", "cron", nil),
		},
		Frontends: []string{"web"},
	}
}

func TestJobsAndCronsShareOneDashboardAndGetNoneOfTheirOwn(t *testing.T) {
	sp := Build(fleet())
	var processDashboards []string
	for _, d := range sp.Dashboards {
		if strings.HasPrefix(d.ID, "process-") {
			processDashboards = append(processDashboards, d.ID)
		}
	}
	want := []string{"process-admin-api", "process-workers", "process-workspace"}
	if !reflect.DeepEqual(processDashboards, want) {
		t.Fatalf("process dashboards = %v, want only the long-running processes %v", processDashboards, want)
	}
	jobs := dashboardByID(t, sp, "jobs")
	if jobs.Group != GroupJobs {
		t.Errorf("jobs group = %q", jobs.Group)
	}
	body, _ := json.Marshal(jobs)
	for _, name := range []string{"db-migrate-up", "dbrole-provision-reliant", "cloudsql-backup-prune"} {
		if !strings.Contains(string(body), name) {
			t.Errorf("the Jobs dashboard never mentions %s", name)
		}
	}
	for _, name := range []string{"admin-api", "workers", "workspace"} {
		if strings.Contains(string(body), `"`+name+`"`) {
			t.Errorf("the Jobs dashboard includes the long-running process %s", name)
		}
	}
}

func TestNoJobsMeansNoJobsDashboard(t *testing.T) {
	in := fleet()
	in.Processes = in.Processes[:3]
	for _, id := range dashboardIDs(Build(in)) {
		if id == "jobs" {
			t.Fatal("a project with no job or cron gets no Jobs dashboard")
		}
	}
}

func TestJobsDashboardContents(t *testing.T) {
	jobs := dashboardByID(t, Build(fleet()), "jobs")
	for _, id := range []string{"jobs-summary", "runs-started", "runs-succeeded", "runs-failed", "runs-by-job", "failures-by-job", "run-events", "restarts", "warning-events", "error-logs"} {
		if !hasPanel(jobs, id) {
			t.Errorf("Jobs dashboard lacks panel %q; has %v", id, panelIDs(jobs))
		}
	}
	// A job exits before the runtime exporter's first export, so go-runtime
	// panels never get data; and a job serves no RPC.
	for _, p := range jobs.Panels {
		if strings.HasPrefix(p.ID, "go-") {
			t.Errorf("job dashboard carries go-runtime panel %q", p.ID)
		}
		for _, s := range p.Series {
			if strings.HasPrefix(s.Field, "rpc.server.") || strings.HasPrefix(s.Field, "go.") {
				t.Errorf("job panel %q reads %s", p.ID, s.Field)
			}
		}
	}
	if usesMetric(jobs, "k8s.container.cpu_limit_utilization") || usesMetric(jobs, "k8s.container.memory_limit_utilization") {
		t.Error("jobs declare no limit-utilization panel")
	}
	// The per-job table names its jobs by forge name, with run durations.
	summary, _ := findPanel(jobs, "jobs-summary")
	if summary.Kind != KindSQL {
		t.Fatalf("summary kind = %q", summary.Kind)
	}
	for _, want := range []string{"'cloudsql-backup-prune'", "'db-migrate-up'", "'dbrole-provision-reliant'", "last_started", "avg_duration_s", "failed", "{from:DateTime64(9)}", "k8s.object.kind"} {
		if !strings.Contains(summary.SQL, want) {
			t.Errorf("summary SQL lacks %q:\n%s", want, summary.SQL)
		}
	}
	// One series per job on the per-job lines, matched on the run name shape.
	byJob, _ := findPanel(jobs, "runs-by-job")
	var labels []string
	for _, s := range byJob.Series {
		labels = append(labels, s.Label)
		if len(s.Where) != 1 || s.Where[0].Op != OpMatch || !strings.HasPrefix(s.Where[0].Value, "^"+s.Label) {
			t.Errorf("series %s is not scoped to its own job's runs: %+v", s.Label, s.Where)
		}
	}
	if want := []string{"cloudsql-backup-prune", "db-migrate-up", "dbrole-provision-reliant"}; !reflect.DeepEqual(labels, want) {
		t.Errorf("per-job series = %v, want %v", labels, want)
	}
	// The run events must be Job events: a pod's own events would count twice.
	started, _ := findPanel(jobs, "runs-started")
	var kind bool
	for _, w := range started.Where {
		if w.Field == "resource.attr.k8s.object.kind" && w.Value == "Job" {
			kind = true
		}
	}
	if !kind {
		t.Errorf("runs-started is not restricted to Job objects: %+v", started.Where)
	}
}

// Every generated dashboard passes the validator readers hold it to.
func TestEveryGeneratedDashboardValidates(t *testing.T) {
	for _, in := range []Input{fleet(), {Project: "shop"}, {Project: "shop", Frontends: []string{"web"}}} {
		for _, d := range Build(in).Dashboards {
			b, _ := json.Marshal(d)
			if _, err := ParseDashboard(d.ID, b); err != nil {
				t.Errorf("%s: %v", d.ID, err)
			}
		}
	}
	many := fleet()
	for i := 0; i < 23; i++ {
		n := "batch-job-with-a-long-name-" + string(rune('a'+i))
		many.Processes = append(many.Processes, Process{Name: n, Kind: "job", Binary: "app", ServiceNames: []string{n}, Workloads: []string{n}})
	}
	d := dashboardByID(t, Build(many), "jobs")
	b, _ := json.Marshal(d)
	if _, err := ParseDashboard("jobs", b); err != nil {
		t.Fatalf("a Jobs dashboard with many jobs must still validate: %v", err)
	}
	if !hasPanel(d, "runs-by-job-3") {
		t.Errorf("26 jobs split over three per-job panels of at most ten series: %v", panelIDs(d))
	}
}

func TestPanelsFollowWhatTheProcessDoes(t *testing.T) {
	sp := Build(fleet())
	api := dashboardByID(t, sp, "process-admin-api")
	workers := dashboardByID(t, sp, "process-workers")
	operator := dashboardByID(t, sp, "process-workspace")

	// RPC panels only for a process that serves RPCs.
	if !usesMetric(api, "rpc.server.calls") || !hasPanel(api, "red-by-rpc") {
		t.Errorf("a service that serves RPCs has RPC panels: %v", panelIDs(api))
	}
	for _, d := range []Dashboard{workers, operator} {
		if usesMetric(d, "rpc.server.calls") || usesMetric(d, "rpc.server.duration") || hasPanel(d, "red-by-rpc") {
			t.Errorf("%s serves no RPC but has RPC panels: %v", d.ID, panelIDs(d))
		}
		for _, f := range d.Filters {
			if strings.HasPrefix(f.ID, "rpc-") {
				t.Errorf("%s has the RPC filter %s", d.ID, f.ID)
			}
		}
	}
	// Worker-run panels only for workers and operators.
	for _, d := range []Dashboard{workers, operator} {
		if !hasPanel(d, "worker-runs") || !hasPanel(d, "worker-failures") {
			t.Errorf("%s runs workers but has no worker panels: %v", d.ID, panelIDs(d))
		}
	}
	if hasPanel(api, "worker-runs") {
		t.Error("a service has no worker-run panel")
	}
	// Limit panels only for a declared limit; the request is the fallback.
	if !usesMetric(api, "k8s.container.cpu_limit_utilization") || !usesMetric(api, "k8s.container.memory_limit_utilization") {
		t.Error("api declares both limits and draws both")
	}
	if !usesMetric(workers, "k8s.container.cpu_limit_utilization") || usesMetric(workers, "k8s.container.memory_limit_utilization") || !usesMetric(workers, "k8s.container.memory_request_utilization") {
		t.Errorf("workers declare a CPU limit only: %v", panelIDs(workers))
	}
	if usesMetric(operator, "k8s.container.cpu_limit_utilization") || usesMetric(operator, "k8s.container.memory_limit_utilization") {
		t.Error("a request-only process has no limit panel")
	}
	if !usesMetric(operator, "k8s.container.cpu_request_utilization") || !hasPanel(operator, "restarts") {
		t.Errorf("a request-only process is drawn against its request and still shows restarts: %v", panelIDs(operator))
	}
	// Every dashboard keeps runtime, logs and a database section only with a DB.
	if !hasPanel(api, "db-latency") || hasPanel(workers, "db-latency") {
		t.Error("database panels only for a process that reaches the database")
	}
	for _, d := range []Dashboard{api, workers, operator} {
		if !hasPanel(d, "go-memory") || !hasPanel(d, "logs") {
			t.Errorf("%s lacks runtime/log panels", d.ID)
		}
	}
}

func TestNoNodeAgentMeansNoSaturationPanels(t *testing.T) {
	in := fleet()
	for i := range in.Processes {
		in.Processes[i].NodeMetadata = false
	}
	for _, d := range Build(in).Dashboards {
		if !strings.HasPrefix(d.ID, "process-") {
			continue
		}
		for _, p := range d.Panels {
			if p.Section == "workload" {
				t.Errorf("%s has workload panel %q although the process runs where no node agent reports", d.ID, p.ID)
			}
		}
	}
}

func TestOverviewScopesEachPanelToTheProcessesItCanHaveDataFor(t *testing.T) {
	sp := Build(fleet())
	ov := dashboardByID(t, sp, "overview")
	if ov.Group != GroupOverview {
		t.Errorf("overview group = %q", ov.Group)
	}
	scope := func(id string) []string {
		p, ok := findPanel(ov, id)
		if !ok {
			t.Fatalf("overview lacks %q: %v", id, panelIDs(ov))
		}
		return p.Where[0].Values
	}
	if got := scope("requests"); !reflect.DeepEqual(got, []string{"admin-api"}) {
		t.Errorf("RPC panels are scoped to the processes that serve RPCs, got %v", got)
	}
	if got := scope("cpu-vs-limit"); !reflect.DeepEqual(got, []string{"admin-api", "workers"}) {
		t.Errorf("cpu-vs-limit scope = %v, want the workloads declaring a CPU limit", got)
	}
	if got := scope("memory-vs-limit"); !reflect.DeepEqual(got, []string{"admin-api"}) {
		t.Errorf("memory-vs-limit scope = %v", got)
	}
	if got := scope("restarts"); !reflect.DeepEqual(got, []string{"admin-api", "workers", "workspace"}) {
		t.Errorf("restarts scope = %v, want the long-running processes and no job", got)
	}
	body, _ := json.Marshal(ov)
	if strings.Contains(string(body), "db-migrate-up") {
		t.Error("the overview is the long-running processes; jobs are on the Jobs dashboard")
	}
}

func TestOverviewWithoutRPCsOrLimitsDropsThoseGroupsOfPanels(t *testing.T) {
	in := Input{Project: "shop", Processes: []Process{{Name: "worker", Kind: "worker", Binary: "app", ServiceNames: []string{"worker"}, Workloads: []string{"worker"}, NodeMetadata: true}}}
	ov := dashboardByID(t, Build(in), "overview")
	for _, id := range []string{"requests", "errors", "p95", "request-rate", "error-rate", "latency", "top-errors", "cpu-vs-limit", "memory-vs-limit"} {
		if hasPanel(ov, id) {
			t.Errorf("overview has %q although nothing serves RPCs or declares a limit", id)
		}
	}
	if !hasPanel(ov, "restarts") || !hasPanel(ov, "error-logs") {
		t.Errorf("restarts and error logs still apply: %v", panelIDs(ov))
	}
}

func TestEveryGeneratedDashboardHasAGroup(t *testing.T) {
	want := map[string]Group{
		"overview": GroupOverview, "process-admin-api": GroupServices, "process-workers": GroupServices, "process-workspace": GroupServices,
		"jobs": GroupJobs, "workloads": GroupPlatform, "releases": GroupReleases, "frontend": GroupProduct,
	}
	got := map[string]Group{}
	for _, d := range Build(fleet()).Dashboards {
		got[d.ID] = d.Group
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groups = %v, want %v", got, want)
	}
}

func TestProcessDashboardIDIsTheForgeWorkloadName(t *testing.T) {
	in := Input{Project: "shop", Processes: []Process{{
		Name: "identity-canary", Kind: "worker", Binary: "control-plane", ServiceNames: []string{"identity-canary"}, Workloads: []string{"identity-canary"},
		Replaces: []string{"control-plane-identity-canary---admin-api-url-http-admin-api.control-plane-prod.svc.cluster.local-80-88a5f8616223", "identity-canary"},
	}}}
	d := dashboardByID(t, Build(in), "process-identity-canary")
	if d.Title != "shop: identity-canary" {
		t.Errorf("title = %q", d.Title)
	}
	want := []string{"process-control-plane-identity-canary---admin-api-url-http-admin-api.control-plane-prod.svc.cluster.local-80-88a5f8616223"}
	if !reflect.DeepEqual(d.Replaces, want) {
		t.Errorf("replaces = %v, want the old generated id (and not its own)", d.Replaces)
	}
	if strings.Contains(d.Description, "--") {
		t.Errorf("description carries rendered arguments: %q", d.Description)
	}
	b, _ := json.Marshal(d)
	if _, err := ParseDashboard(d.ID, b); err != nil {
		t.Fatalf("a dashboard replacing a long old id must validate: %v", err)
	}
}

func TestWithoutProcessDashboardsKeepsOnlyTheFrontend(t *testing.T) {
	sp := WithoutProcessDashboards(Build(Input{Project: "shop", Frontends: []string{"web"}}))
	if got := dashboardIDs(sp); !reflect.DeepEqual(got, []string{"frontend"}) {
		t.Fatalf("dashboards = %v", got)
	}
}

func TestFrontendDashboardFollowsTheServicesTheEnvServes(t *testing.T) {
	in := Input{Project: "control-plane", Frontends: []string{"reliant-web"}, FrontendServices: []string{"control-plane-reliant-web", "reliant-web"}}
	fe := dashboardByID(t, Build(in), "frontend")
	p, _ := findPanel(fe, "js-errors")
	var scope []string
	for _, w := range p.Where {
		if w.Field == FieldService {
			scope = w.Values
		}
	}
	if !reflect.DeepEqual(scope, []string{"control-plane-reliant-web", "reliant-web"}) {
		t.Errorf("frontend scope = %v", scope)
	}
	if strings.Contains(mustMarshal(t, fe), "internal-console") {
		t.Error("the frontend dashboard names a frontend the env does not serve")
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestJobsDashboardIsNotWrittenIntoTheProjectTree(t *testing.T) {
	dir := t.TempDir()
	written, err := Write(dir, fleet(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range written {
		if strings.Contains(w, "-jobs_gen") || strings.Contains(w, "db-migrate-up") || strings.Contains(w, "process-db-migrate") {
			t.Errorf("Write emitted %s: jobs read cluster events a local stack lacks, and a job has no dashboard of its own", w)
		}
	}
}
