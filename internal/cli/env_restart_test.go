package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/procguard"
	deployv1alpha1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// TestInspectRestartStack pins what a restart reads from the process table:
// whether the stack is running at all, and which run the replacement joins —
// the replaced process's own run first, else another live process's.
func TestInspectRestartStack(t *testing.T) {
	f := fakeFacts{env: map[int][]string{
		300: runMarker(testProj, "dev", "worker", "SUPERVISOR"),
		400: runMarker(testProj, "dev", "api", "SCOPED"),
		500: runMarker("projB", "dev", "api", "FOREIGN"),
		600: runMarker(testProj, "staging", "api", "STAGING"),
		700: runMarker(testProj, "dev", "frontend:web", "WEB"),
	}}
	pids := []int{700, 600, 500, 400, 300}

	if got := inspectRestartStack(testProj, "dev", []string{"api"}, pids, f, false); !got.running || got.runID != "SCOPED" {
		t.Errorf("api alive: %+v, want running and the api process's run SCOPED", got)
	}
	if got := inspectRestartStack(testProj, "dev", []string{"web"}, pids, f, false); got.runID != "WEB" {
		t.Errorf("frontend target: run %q, want WEB (its frontend: marker matches the bare name)", got.runID)
	}
	// The api is dead: join the run another process of the stack carries.
	delete(f.env, 400)
	if got := inspectRestartStack(testProj, "dev", []string{"api"}, []int{700, 600, 500, 300}, f, false); !got.running || got.runID != "SUPERVISOR" {
		t.Errorf("api dead: %+v, want running and the lowest live pid's run SUPERVISOR", got)
	}
	// Nothing of this project+env is alive: running only on a launch record,
	// and never under another project's or env's run.
	only := []int{500, 600}
	if got := inspectRestartStack(testProj, "dev", []string{"api"}, only, f, true); !got.running || got.runID != "" {
		t.Errorf("recorded, nothing alive: %+v, want running with a fresh run", got)
	}
	if got := inspectRestartStack(testProj, "dev", []string{"api"}, only, f, false); got.running {
		t.Errorf("no record, nothing alive: %+v, want not running", got)
	}
}

func restartTestEntities() *KCLEntities {
	host := func(name string) WorkloadEntity {
		return WorkloadEntity{Name: name, Kind: "service", Runtime: RuntimeEntity{Type: RuntimeHost, Host: &HostRuntime{}}}
	}
	job := host("migrate")
	job.Kind = "job"
	edge := WorkloadEntity{Name: "edge", Kind: "service", Runtime: RuntimeEntity{Type: RuntimeCluster}}
	return &KCLEntities{
		Workloads: []WorkloadEntity{host("api"), job, edge},
		Frontends: []FrontendEntity{{Name: "web"}},
	}
}

// TestRestartChecksTargets: a restart refuses, before stopping anything,
// whatever is not a process `forge env up` supervises, and names the verb
// that converges it instead. A plain `forge env up` (nil restart) is never
// affected.
func TestRestartChecksTargets(t *testing.T) {
	e := restartTestEntities()
	if !e.Workloads[0].LongRunning() || e.Workloads[1].LongRunning() {
		t.Fatal("fixture: api must be long-running and migrate a job")
	}
	r := &upRestart{}
	if err := r.checkTargets(e, "dev", []string{"api", "web"}); err != nil {
		t.Errorf("host service + frontend: %v, want accepted", err)
	}
	err := r.checkTargets(e, "dev", []string{"api", "edge", "migrate"})
	if err == nil {
		t.Fatal("cluster workload and job accepted")
	}
	for _, want := range []string{"nothing was stopped", "edge runs on the cluster runtime", "`forge env up dev --target edge`", "migrate is a job"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "api ") {
		t.Errorf("refusal names the restartable api: %q", err)
	}
	var plain *upRestart
	if err := plain.checkTargets(e, "dev", []string{"edge"}); err != nil {
		t.Errorf("forge env up --target edge refused: %v", err)
	}
	if !plain.runsJobs() || !plain.joinsSession() || r.runsJobs() || r.joinsSession() {
		t.Error("only a restart skips the jobs and the session")
	}
}

// TestReuseLivePortsKeepsTheStacksPorts: a scoped run binds the ports the
// running stack recorded for every workload with no declared port, so the
// processes it leaves running (a frontend wired to the API's port) still
// reach the ones it replaces. Without it the API came back on a fresh
// ephemeral port.
func TestReuseLivePortsKeepsTheStacksPorts(t *testing.T) {
	declared := WorkloadEntity{Name: "admin", Kind: "service", Runtime: RuntimeEntity{Type: RuntimeHost, Host: &HostRuntime{ListenPorts: &[]int{9100}}}}
	silent := WorkloadEntity{Name: "worker", Kind: "service", Runtime: RuntimeEntity{Type: RuntimeHost, Host: &HostRuntime{ListenPorts: &[]int{}}}}
	e := restartTestEntities()
	e.Workloads = append([]WorkloadEntity{e.Workloads[0], declared, silent}, e.Workloads[1:]...)
	cfg := &config.ProjectConfig{Frontends: []config.FrontendConfig{{Name: "web"}}}
	st := &resolvedEnvState{Ports: map[string]int{"api": 41234, "admin": 1, "worker": 2, "web": 41235}}

	captureStdout(t, func() { reuseLivePorts(e, cfg, st) })
	if got := e.Workloads[0].HostPort(); got != 41234 {
		t.Errorf("api port = %d, want the recorded 41234", got)
	}
	if got := e.Workloads[0].Runtime.Host.LaunchEnv["PORT"]; got != "41234" {
		t.Errorf("api PORT = %q, want 41234: the process must bind the port forge publishes", got)
	}
	if got := e.Workloads[1].HostPort(); got != 9100 {
		t.Errorf("declared admin port = %d, want its declared 9100", got)
	}
	if got := *e.Workloads[2].Runtime.Host.ListenPorts; len(got) != 0 {
		t.Errorf("worker binds nothing, got ports %v", got)
	}
	if e.Frontends[0].Port != 41235 || cfg.Frontends[0].Port != 41235 {
		t.Errorf("web port = %d (cfg %d), want 41235", e.Frontends[0].Port, cfg.Frontends[0].Port)
	}
	var url string
	captureStdout(t, func() { url = resolveEphemeralHostPorts(e) })
	if url != "http://localhost:41234" {
		t.Errorf("frontend API URL = %q, want the kept api port", url)
	}
}

// TestScopedLaunchRecordKeepsTheRestOfTheStack: the record a scoped run writes
// still names the ports of the processes it left running, so `forge env
// status` keeps reporting and probing them; it used to keep only the targets.
// It also records the -D options a restart renders with.
func TestScopedLaunchRecordKeepsTheRestOfTheStack(t *testing.T) {
	dir, projectID, env := testStack(t)
	_ = dir
	path, err := upEnvStatePath(projectID, env)
	if err != nil {
		t.Fatal(err)
	}
	prev, _ := json.Marshal(resolvedEnvState{Env: env, Ports: map[string]int{"api": 41000, "web": 41001}})
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, prev, 0o644); err != nil {
		t.Fatal(err)
	}
	api := WorkloadEntity{Name: "api", Kind: "service", Runtime: RuntimeEntity{Type: RuntimeHost, Host: &HostRuntime{ListenPorts: &[]int{42000}}}}
	e := &KCLEntities{Workloads: []WorkloadEntity{api}}

	persistResolvedEnv(projectID, env, e, nil, []string{"api"}, false, []string{"host_runner=go-run"})
	st := loadResolvedEnv(projectID, env)
	if st == nil {
		t.Fatal("no launch record written")
	}
	if st.Ports["api"] != 42000 || st.Ports["web"] != 41001 {
		t.Errorf("ports = %v, want api's new 42000 and web's kept 41001", st.Ports)
	}
	if !slices.Equal(st.RenderOptions, []string{"host_runner=go-run"}) {
		t.Errorf("render options = %v", st.RenderOptions)
	}

	// A full run replaced the whole stack: its rows are the record.
	persistResolvedEnv(projectID, env, e, nil, nil, false, nil)
	if st := loadResolvedEnv(projectID, env); len(st.Ports) != 1 || st.RenderOptions != nil {
		t.Errorf("full run record = %+v, want only api and no options", st)
	}
}

// TestRestartHints: `forge env status` names the restart for each host
// process or frontend of a recorded stack that stopped listening.
func TestRestartHints(t *testing.T) {
	rows := []upServiceRow{
		{Name: "api", Kind: "host", Port: 41000},
		{Name: "web", Kind: "frontend", Port: 41001},
		{Name: "admin", Kind: "host", Port: 41002, Listening: true},
		{Name: "worker", Kind: "host"},                          // no port: no probe to read
		{Name: "proxy", Kind: "host", Port: 41003, HeldBy: "x"}, // another stack's port
	}
	got := restartHints("dev", rows, &resolvedEnvState{})
	want := "api, web not listening — restart: forge env restart dev api web"
	if len(got) != 1 || got[0] != want {
		t.Errorf("hints = %q, want [%q]", got, want)
	}
	if got := restartHints("dev", rows, nil); got != nil {
		t.Errorf("a stack with no launch record got %q; `forge env up` is the answer there", got)
	}
}

// TestHoldOutlivesARestart: a supervising `forge env up` keeps holding while
// a restart replaces the only process it started, and while the replacement
// (which carries its run marker) runs; it ends when neither is left.
func TestHoldOutlivesARestart(t *testing.T) {
	old := holdPoll
	holdPoll = 5 * time.Millisecond
	t.Cleanup(func() { holdPoll = old })
	_, projectID, env := testStack(t)

	exited := make(chan struct{})
	close(exited)
	reg := newProcRegistry(projectID, t.TempDir(), env)
	reg.processes = []*managedProcess{{name: "api", pid: 110, done: exited}}
	reg.selfLineage = func() procguard.Lineage { return procguard.Lineage{} }
	var replacement atomic.Bool
	reg.readStopTable = func() (*stopTable, error) {
		rows := map[int]stopProc{1: {}}
		env := map[int][]string{}
		if replacement.Load() {
			rows[220] = stopProc{ppid: 1}
			env[220] = runMarker(projectID, reg.env, "api", reg.runID)
		}
		return fakeStopTable(rows, env), nil
	}

	release, err := takeRestartLease(projectID, env)
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan bool, 1)
	go func() { ended <- awaitHoldEnd(t.Context(), reg) }()
	assertHolding := func(why string) {
		t.Helper()
		select {
		case <-ended:
			t.Fatalf("the hold ended %s", why)
		case <-time.After(60 * time.Millisecond):
		}
	}
	assertHolding("while a restart held its lease")
	replacement.Store(true)
	release()
	assertHolding("while the restarted process runs")
	replacement.Store(false)
	select {
	case gone := <-ended:
		if !gone {
			t.Error("awaitHoldEnd = false, want true: nothing of the run is left")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hold did not end once the replacement was gone")
	}
}

// TestRestartLeaseHeld: only a live, recent restart holds the lease.
func TestRestartLeaseHeld(t *testing.T) {
	_, projectID, env := testStack(t)
	now := time.Now()
	if restartLeaseHeld(projectID, env, now) {
		t.Fatal("held with no lease")
	}
	release, err := takeRestartLease(projectID, env)
	if err != nil {
		t.Fatal(err)
	}
	if !restartLeaseHeld(projectID, env, now) {
		t.Error("a live restart's lease is not held")
	}
	if restartLeaseHeld(projectID, env, now.Add(restartLeaseMaxAge+time.Minute)) {
		t.Error("an hour-old lease is still held; a recycled pid could hold a supervisor forever")
	}
	release()
	if restartLeaseHeld(projectID, env, now) {
		t.Error("held after release")
	}
	path, _ := restartLeasePath(projectID, env)
	dead, _ := json.Marshal(restartLease{PID: 1 << 30, StartedAt: now})
	if err := os.WriteFile(path, dead, 0o644); err != nil {
		t.Fatal(err)
	}
	if restartLeaseHeld(projectID, env, now) {
		t.Error("a dead restart's lease is held")
	}
}

// TestHostStopGraceHonoursAirKillDelay: the stop grace a launch records — the
// wait `forge env down` and a restart give a process before SIGKILL — covers
// an Air workload's kill_delay, as the supervising run's own shutdown does.
func TestHostStopGraceHonoursAirKillDelay(t *testing.T) {
	airCfg := filepath.Join(t.TempDir(), ".air.toml")
	if err := os.WriteFile(airCfg, []byte("[build]\nsend_interrupt = true\nkill_delay = \"12s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	air := WorkloadEntity{Name: "api", Kind: "service", Runtime: RuntimeEntity{Type: RuntimeHost, Host: &HostRuntime{Runner: "air", AirConfig: airCfg}}}
	if got, want := hostStopGrace(&KCLEntities{Workloads: []WorkloadEntity{air}}), 12*time.Second+stopGraceMargin; got != want {
		t.Errorf("air kill_delay 12s: grace %s, want %s", got, want)
	}
	n := int32(20)
	air.Spec = deployv1alpha1.WorkloadSpec{PreStopSeconds: &n}
	if got, want := hostStopGrace(&KCLEntities{Workloads: []WorkloadEntity{air}}), 20*time.Second+stopGraceMargin; got != want {
		t.Errorf("preStopSeconds 20 beside kill_delay 12s: grace %s, want %s", got, want)
	}
}

// TestEnvRestartRefusesAStackThatIsNotRunning: a restart replaces processes
// of a running stack; with no live process and no launch record it starts
// nothing and names `forge env up`.
func TestEnvRestartRefusesAStackThatIsNotRunning(t *testing.T) {
	dir, _, env := testStack(t)
	t.Chdir(dir)
	err := runEnvRestart(&cobra.Command{}, env, []string{"api"}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "is not running in this project") || !strings.Contains(err.Error(), "forge env up "+env) {
		t.Fatalf("err = %v, want the not-running refusal naming forge env up", err)
	}
}
