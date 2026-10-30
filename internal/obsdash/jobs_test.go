package obsdash

import (
	"encoding/json"
	"reflect"
	"regexp"
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
	for _, id := range []string{"jobs-summary", "runs-totals", "runs-by-job", "failures-by-job", "recent-runs", "restarts", "warning-events", "error-logs"} {
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
	// The per-job charts and the totals count EXECUTIONS (one Job in one
	// environment), not events: they are SQL panels over the grouped runs.
	for _, id := range []string{"runs-totals", "runs-by-job", "failures-by-job"} {
		p, _ := findPanel(jobs, id)
		if p.Kind != KindSQL || !strings.Contains(p.SQL, "GROUP BY env, run, uid, generation") {
			t.Errorf("%s must count distinct (env, Job) executions, got kind %q:\n%s", id, p.Kind, p.SQL)
		}
	}
	// The run list reads Job events only: a pod's own events would count twice.
	recent, _ := findPanel(jobs, "recent-runs")
	if recent.Kind != KindSQL || !strings.Contains(recent.SQL, "= 'Job'") {
		t.Errorf("recent-runs is not restricted to Job objects:\n%s", recent.SQL)
	}
}

// The Job controller emits SuccessfulCreate once per POD: a parallel Job, or a
// pod replaced after a failure, repeats it for one run. No panel may count
// those events as runs; every run count is over distinct executions.
func TestJobsDashboardNeverCountsCreateEventsAsRuns(t *testing.T) {
	jobs := dashboardByID(t, Build(fleet()), "jobs")
	for _, p := range jobs.Panels {
		counts := false
		for _, s := range p.Series {
			counts = counts || s.Agg == AggCount
		}
		for _, w := range p.Where {
			if counts && w.Field == fieldEventWhy && strings.Contains(strings.Join(append(w.Values, w.Value), " "), jobEventCreated) {
				t.Errorf("panel %q counts %s events, which are per pod, not per run", p.ID, jobEventCreated)
			}
		}
	}
	started, ok := findPanel(jobs, "runs-totals")
	if !ok || !strings.Contains(started.SQL, "GROUP BY env, run, uid, generation") || strings.Contains(started.SQL, "count() AS runs_started") {
		t.Errorf("runs started must count executions:\n%s", started.SQL)
	}
}

// Job runs and their pods carry different k8s.workload.name values. The node
// agent takes the pod's app.kubernetes.io/name label first (forge renders it as
// the bare forge name on every Job and CronJob pod), then the owner chain; an
// event about a Job OBJECT is named for the object (forge name + run suffix).
// The restarts panel must select every one of these, and nothing that merely
// starts with a job's name. The Warning-event panel resolves the same names in
// SQL (see TestJobsWarningsSQLResolvesBothObjectKinds).
func TestJobsHealthPanelsMatchRealRunIdentities(t *testing.T) {
	jobs := dashboardByID(t, Build(fleet()), "jobs")
	type identity struct {
		what, workload string
		want           bool
	}
	for _, id := range []identity{
		// a standalone Job (forge: <name>-<10 hex spec hash>)
		{"Job object event, spec-hashed name", "db-migrate-up-0123456789", true},
		{"its pod, via the app.kubernetes.io/name label", "db-migrate-up", true},
		{"its pod, label-less: the owner chain names the Job", "db-migrate-up-a1b2c3d4e5", true},
		{"another job's hashed run", "dbrole-provision-reliant-aaaaaaaaaa", true},
		// a CronJob (<name>-<scheduled minutes since epoch>)
		{"cron-spawned Job object event", "cloudsql-backup-prune-29012345", true},
		{"cron pod via the label", "cloudsql-backup-prune", true},
		{"cron pod, label-less: the owner chain names the CronJob", "cloudsql-backup-prune", true},
		// not this project's jobs
		{"another project's job", "other-thing-0123456789", false},
		{"a longer job name sharing the prefix", "db-migrate-up-worker", false},
		{"a long-running workload sharing the prefix", "db-migrate-up-api-0123456789", false},
		{"an unresolved pod name", "db-migrate-up-0123456789-x7k2p", false},
		{"a prefix of a job name", "db-migrate", false},
	} {
		for _, panelID := range []string{"restarts-1", "restarts"} {
			p, ok := findPanel(jobs, panelID)
			if !ok {
				continue
			}
			var re *regexp.Regexp
			for _, w := range p.Where {
				if w.Field == fieldWorkload {
					if w.Op != OpMatch {
						t.Fatalf("%s selects workloads with %q, which cannot match a suffixed run name", panelID, w.Op)
					}
					re = regexp.MustCompile(w.Value)
				}
			}
			if re == nil {
				t.Fatalf("%s has no workload scope: %+v", panelID, p.Where)
			}
			if got := re.MatchString(id.workload); got != id.want {
				t.Errorf("%s: %s (%q) matched = %v, want %v (regex %s)", panelID, id.what, id.workload, got, id.want, re)
			}
		}
	}
}

// A project with many jobs cannot fit one spec regex (256 bytes): the health
// panels split by job group, and every job lands in exactly one group.
func TestJobsHealthPanelsSplitWhenTheNamesDoNotFitOneRegex(t *testing.T) {
	many := fleet()
	var all []string
	for i := 0; i < 23; i++ {
		n := "batch-job-with-a-long-name-" + string(rune('a'+i))
		all = append(all, n)
		many.Processes = append(many.Processes, Process{Name: n, Kind: "job", Binary: "app", ServiceNames: []string{n}, Workloads: []string{n}})
	}
	d := dashboardByID(t, Build(many), "jobs")
	var res []*regexp.Regexp
	for _, p := range d.Panels {
		if !strings.HasPrefix(p.ID, "restarts-") {
			continue
		}
		for _, w := range p.Where {
			if w.Field == fieldWorkload {
				if len(w.Value) > maxPredicateRegex {
					t.Errorf("%s regex is %d bytes, over the spec's %d", p.ID, len(w.Value), maxPredicateRegex)
				}
				res = append(res, regexp.MustCompile(w.Value))
			}
		}
	}
	if len(res) < 2 {
		t.Fatalf("26 jobs must not share one regex: %v", panelIDs(d))
	}
	for _, n := range append(all, "db-migrate-up", "dbrole-provision-reliant", "cloudsql-backup-prune") {
		hits := 0
		for _, re := range res {
			if re.MatchString(n) && re.MatchString(n+"-0123456789") {
				hits++
			}
		}
		if hits != 1 {
			t.Errorf("job %s is covered by %d health groups, want 1", n, hits)
		}
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
	for _, id := range []string{"runs-by-job", "failures-by-job", "restarts-2", "warning-events", "recent-runs"} {
		if !hasPanel(d, id) {
			t.Errorf("26 jobs: Jobs dashboard lacks %q: %v", id, panelIDs(d))
		}
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

// The Jobs dashboard names every per-job dashboard it replaces, under the id
// each had before and the id it would have had under the workload name, so a
// reader can move stars and copies of any of them here.
func TestJobsDashboardReplacesTheOldPerJobDashboards(t *testing.T) {
	in := Input{Project: "shop", Processes: []Process{
		{Name: "db-migrate-up", Kind: "job", Binary: "app", ServiceNames: []string{"db-migrate-up"}, Workloads: []string{"db-migrate-up"}, Replaces: []string{"app-db-migrate-up"}},
		{Name: "cloudsql-backup-prune", Kind: "cron", Binary: "app", ServiceNames: []string{"x"}, Workloads: []string{"cloudsql-backup-prune"}, Replaces: []string{"app-cloudsql-prune-backups---project-p-bcbc04b8f7e4"}},
	}}
	d := dashboardByID(t, Build(in), "jobs")
	want := []string{
		"process-app-cloudsql-prune-backups---project-p-bcbc04b8f7e4", "process-app-db-migrate-up",
		"process-cloudsql-backup-prune", "process-db-migrate-up",
	}
	if !reflect.DeepEqual(d.Replaces, want) {
		t.Fatalf("replaces = %v, want %v", d.Replaces, want)
	}
	b, _ := json.Marshal(d)
	if _, err := ParseDashboard("jobs", b); err != nil {
		t.Fatalf("a Jobs dashboard with replaces must validate: %v", err)
	}
	for _, id := range d.Replaces {
		if id == "jobs" {
			t.Error("a dashboard never replaces itself")
		}
	}
}

// A run is one Kubernetes Job in one ENVIRONMENT: identical specs deployed to
// two environments share a hashed Job name, and must not collapse into one run.
func TestJobsSummaryGroupsRunsByEnvironmentAndJob(t *testing.T) {
	sql := jobsSummarySQL([]Process{{Name: "db-migrate-up", Kind: "job", ServiceNames: []string{"db-migrate-up"}, Workloads: []string{"db-migrate-up"}}})
	if !strings.Contains(sql, "GROUP BY env, run, uid, generation") || !strings.Contains(sql, "ResourceAttributes['reliant.env.id'] AS env") {
		t.Errorf("the per-run grouping must carry the environment, the Job UID and the generation:\n%s", sql)
	}
	if !strings.Contains(sql, "ResourceAttributes['k8s.object.uid'] AS uid") {
		t.Errorf("the Job UID must come from the involved object's UID:\n%s", sql)
	}
	if !strings.Contains(sql, "has({env_ids:Array(String)}, ResourceAttributes['reliant.env.id'])") {
		t.Errorf("the summary must honour the dashboard's environments:\n%s", sql)
	}
}

// A standalone Job's name is its pod-spec hash alone, so recreating an
// unchanged Job reuses it. Every run count is built on the same executions
// subquery, so none of them can drift from the summary.
func TestEveryRunCountSharesTheExecutionGrouping(t *testing.T) {
	jobs := dashboardByID(t, Build(fleet()), "jobs")
	for _, id := range []string{"jobs-summary", "runs-totals", "runs-by-job", "failures-by-job", "recent-runs"} {
		p, ok := findPanel(jobs, id)
		if !ok {
			t.Fatalf("no panel %s: %v", id, panelIDs(jobs))
		}
		if !strings.Contains(p.SQL, "GROUP BY env, run, uid, generation") {
			t.Errorf("%s does not count distinct executions (env, name, UID, generation):\n%s", id, p.SQL)
		}
	}
}

// The dashboard declares a service.name filter, and the console applies a
// selection to every panel. A declarative panel is narrowed by the console; a
// SQL panel only by a statement that binds {services:Array(String)}. Every SQL
// panel must bind it, or a selection narrows some panels and not others.
func TestEveryJobsPanelHonoursTheServiceFilter(t *testing.T) {
	jobs := dashboardByID(t, Build(fleet()), "jobs")
	hasFilter := false
	for _, f := range jobs.Filters {
		hasFilter = hasFilter || f.Field == FieldService
	}
	if !hasFilter {
		t.Fatalf("the Jobs dashboard has no service.name filter: %+v", jobs.Filters)
	}
	sqlPanels := 0
	for _, p := range jobs.Panels {
		if p.Kind != KindSQL {
			continue
		}
		sqlPanels++
		if !strings.Contains(p.SQL, "{services:Array(String)}") {
			t.Errorf("SQL panel %s ignores the service filter (never binds {services:Array(String)}):\n%s", p.ID, p.SQL)
		}
		if !strings.Contains(p.SQL, "empty({services:Array(String)})") {
			t.Errorf("SQL panel %s must treat an empty selection as every job:\n%s", p.ID, p.SQL)
		}
		if strings.Contains(p.SQL, "k8s.object.kind") == false && p.ID != "warning-events" {
			t.Errorf("SQL panel %s does not read Job events:\n%s", p.ID, p.SQL)
		}
	}
	if sqlPanels != 6 {
		t.Errorf("Jobs dashboard has %d SQL panels, want the 6 run and warning panels: %v", sqlPanels, panelIDs(jobs))
	}
	// The selector resolves a service.name a workload declares for itself.
	in := Process{Name: "indexer", Kind: "job", ServiceNames: []string{"search-indexer"}, Workloads: []string{"indexer"}}
	sql := jobsSummarySQL([]Process{in})
	for _, want := range []string{"('indexer', 'search-indexer')", "arrayExists(", "has({services:Array(String)}, run)"} {
		if !strings.Contains(sql, want) {
			t.Errorf("the selector does not resolve a declared service.name (%q missing):\n%s", want, sql)
		}
	}
	// A name with a quote cannot break out of its literal.
	evil := Process{Name: "x", Kind: "job", ServiceNames: []string{"o'brien"}, Workloads: []string{"x"}}
	if sql := jobsSummarySQL([]Process{evil}); !strings.Contains(sql, `'o\'brien'`) {
		t.Errorf("a service name must be quoted:\n%s", sql)
	}
}

// The Warning-event panel counts events of the Job itself (named for the Job:
// forge name + run suffix) and of its pods (named for the forge job), and
// nothing else.
func TestJobsWarningsSQLResolvesBothObjectKinds(t *testing.T) {
	sql := jobsWarningsSQL([]Process{{Name: "db-migrate-up", Kind: "job", ServiceNames: []string{"db-migrate-up"}, Workloads: []string{"db-migrate-up"}}})
	for _, want := range []string{"k8s.event.type'] = 'Warning'", "'(" + jobRunSuffix + ")?$'", "['db-migrate-up']"} {
		if !strings.Contains(sql, want) {
			t.Errorf("warnings SQL lacks %q:\n%s", want, sql)
		}
	}
}
