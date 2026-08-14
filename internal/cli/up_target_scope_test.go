package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// targetScopeEntities is a stack with both sides of the split declared: two
// cluster services (which only a full run should build and push), one host
// service, and two frontends. It is the shape that made `--target reliant-web`
// docker-build the whole project.
func targetScopeEntities() *KCLEntities {
	return &KCLEntities{
		Services: []ServiceEntity{
			{Name: "admin-server", Deploy: DeployConfigEntity{Type: "host", Host: &HostDeploy{Runner: "go-run"}}},
			{Name: "workspace-proxy", Deploy: DeployConfigEntity{Type: "cluster"}},
			{Name: "daemon-gateway", Deploy: DeployConfigEntity{Type: "cluster"}},
		},
		Frontends: []FrontendEntity{
			{Name: "reliant-web", Port: 3000},
			{Name: "settings-web", Port: 3001},
		},
	}
}

// TestUpTargetRejectsUnknownName pins the typo case. `forge env up --target`
// used to accept any string: inTargetSet simply matched nothing, so the run
// tore the stack down (the pre-flight is unconditional) and then started
// nothing in its place. The available-name list is what makes the error
// actionable, and it must include frontends — the names most likely to be
// targeted are exactly the ones the deploy-side validator was never asked
// about.
func TestUpTargetRejectsUnknownName(t *testing.T) {
	e := targetScopeEntities()

	if err := validateDeployTargets(e, nil); err != nil {
		t.Errorf("empty target set must be a no-op: %v", err)
	}
	if err := validateDeployTargets(e, []string{"reliant-web"}); err != nil {
		t.Errorf("a declared frontend must be a valid target: %v", err)
	}

	err := validateDeployTargets(e, []string{"reliant-wbe"})
	if err == nil {
		t.Fatal("a target that names nothing was accepted; the run would tear the stack down and start nothing")
	}
	// The message has to name what IS available, or the user is left
	// guessing at the spelling that just cost them their stack.
	for _, want := range []string{"reliant-wbe", "reliant-web", "admin-server"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestUpTargetReachesTheBuildPhase pins the WIRING, not the filter. The
// narrowing helper (filterEntitiesByTarget) already existed and worked — the
// defect was that `forge env up`'s build phase never passed it anything, so
// `--target reliant-web` docker-built and pushed every cluster image on the
// way to starting one Vite dev server. Asserting on the helper alone passes
// even with the wiring removed, so this asserts on the buildOptions the up
// path actually constructs.
func TestUpTargetReachesTheBuildPhase(t *testing.T) {
	// upBuildCluster is the up path's entry into runBuild. Its options are
	// what decide whether the build phase can scope at all; a targets field
	// that never arrives is the bug.
	opts := upBuildOptionsFor("dev", "localhost:5051", false, []string{"reliant-web"})

	if len(opts.targets) != 1 || opts.targets[0] != "reliant-web" {
		t.Fatalf("--target never reached the build phase (buildOptions.targets = %v); "+
			"the docker build+push runs unscoped", opts.targets)
	}

	// And with no target, the build stays unscoped — a bare `forge env up`
	// must still build everything.
	if got := upBuildOptionsFor("dev", "localhost:5051", false, nil); len(got.targets) != 0 {
		t.Errorf("unscoped run picked up targets %v", got.targets)
	}
}

// TestUpTargetScopesTheBuildSet covers what the narrowed set then means for
// the build decisions downstream of it: the go-build targets, the docker
// build+push, and the skipProjectDocker guard all read "does this env still
// declare a cluster service" off the filtered entities.
func TestUpTargetScopesTheBuildSet(t *testing.T) {
	e := targetScopeEntities()

	// Sanity: unfiltered, this env has cluster services, so a full run
	// legitimately builds and pushes an image.
	if !kclHasClusterService(e) {
		t.Fatal("fixture is wrong: the unfiltered env must declare a cluster service")
	}

	scoped := filterEntitiesByTarget(e, []string{"reliant-web"})

	if kclHasClusterService(scoped) || kclHasClusterFrontend(scoped) {
		t.Error("targeting a frontend left an image-shipping entity in the build set — the docker build+push still runs")
	}
	if len(scoped.Frontends) != 1 || scoped.Frontends[0].Name != "reliant-web" {
		t.Errorf("targeted frontend not preserved: %+v", scoped.Frontends)
	}

	// Targeting a cluster service keeps its build: scoping must narrow, not
	// disable, or `--target workspace-proxy` would deploy a stale image.
	clusterScoped := filterEntitiesByTarget(e, []string{"workspace-proxy"})
	if !kclHasClusterService(clusterScoped) {
		t.Error("targeting a cluster service dropped it from the build set")
	}
	if len(clusterScoped.Services) != 1 {
		t.Errorf("expected exactly the targeted service, got %+v", clusterScoped.Services)
	}
}

// TestFilterRootsByService covers the scoped-teardown selection rule without
// real processes: frontends are stamped `frontend:<name>` but targeted by
// bare name, and an unattributable process is never signalled by a scoped
// stop (it is not evidence the targeted service is running).
func TestFilterRootsByService(t *testing.T) {
	facts := &fakeProcFacts{env: map[int][]string{
		10: {forgeUpServiceVar + "=admin-server"},
		11: {forgeUpServiceVar + "=frontend:reliant-web"},
		12: {forgeUpServiceVar + "=workspace-proxy"},
		// 13 has no readable environment at all.
	}}

	got := filterRootsByService([]int{10, 11, 12, 13}, []string{"reliant-web"}, facts)
	if len(got) != 1 || got[0] != 11 {
		t.Errorf("scoped teardown selected %v; want just the frontend pid 11", got)
	}

	got = filterRootsByService([]int{10, 11, 12, 13}, []string{"admin-server", "reliant-web"}, facts)
	if len(got) != 2 {
		t.Errorf("multi-target teardown selected %v; want pids 10 and 11", got)
	}

	if got := filterRootsByService([]int{13}, []string{"admin-server"}, facts); len(got) != 0 {
		t.Errorf("an unattributable process was selected for a scoped teardown: %v", got)
	}
}

// fakeProcFacts serves a fixed environment per pid, with no process table.
type fakeProcFacts struct {
	env map[int][]string
}

func (f *fakeProcFacts) environ(pid int) ([]string, bool) {
	e, ok := f.env[pid]
	return e, ok
}
func (f *fakeProcFacts) parent(int) (int, bool) { return 0, false }
func (f *fakeProcFacts) argv(int) ([]string, bool) {
	return nil, false
}

// TestUpPreflight_ScopedTargetLeavesOtherServicesRunning is the destructive
// half of the defect, pinned against real marked processes.
//
// `forge env up dev --target reliant-web` on a live stack used to SIGTERM
// every service in the env — admin-server, the API server, the worker — and
// then start only the frontend. The user asked to restart one service and
// silently lost five, with nothing in the output saying so.
//
// The predecessor of the TARGETED service must still be stopped: that is the
// "one stack per (project, env)" rule the reclaim exists to enforce, and
// leaving it would put two copies of one service on the same port.
func TestUpPreflight_ScopedTargetLeavesOtherServicesRunning(t *testing.T) {
	requireProcInspection(t)
	dir, projectID, env := testStack(t)

	targeted := spawnMarked(t, projectID, env, "frontend:reliant-web")
	bystander := spawnMarked(t, projectID, env, "admin-server")
	otherFrontend := spawnMarked(t, projectID, env, "frontend:settings-web")

	reg := newProcRegistry(projectID, dir, env)
	reg.processes = []*managedProcess{
		{name: "frontend:reliant-web", pid: targeted.pid(), cmd: &exec.Cmd{}},
		{name: "admin-server", pid: bystander.pid(), cmd: &exec.Cmd{}},
		{name: "frontend:settings-web", pid: otherFrontend.pid(), cmd: &exec.Cmd{}},
	}
	reg.persist()

	port := freePort(t)
	if err := upPreflight(projectID, env, entitiesOnPort(port), []string{"reliant-web"}, true); err != nil {
		t.Fatalf("upPreflight with --target: %v", err)
	}

	if !targeted.waitExit(15 * time.Second) {
		t.Errorf("the targeted service's predecessor (pid %d) survived — two copies now race for its port", targeted.pid())
	}
	if !bystander.alive() {
		t.Error("--target reliant-web killed admin-server: a scoped run must not tear down services it was not asked to restart")
	}
	if !otherFrontend.alive() {
		t.Error("--target reliant-web killed settings-web: only the named service may be replaced")
	}

	// The ledger must still describe the survivors. It is what
	// `forge env down` and `forge env ps` read; dropping it under a scope
	// would strand every process the scoped run deliberately left alive.
	statePath, err := upStatePath(projectID, env)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("scoped teardown removed the ledger describing still-running services: %v", err)
	}
	if !strings.Contains(string(data), "admin-server") {
		t.Errorf("ledger lost the surviving admin-server, stranding it: %q", data)
	}
}

// TestPersistCarriesForwardSurvivingEntries pins the ledger merge. A scoped
// run rewrites the ledger after starting only its own services; without the
// carry-forward it would erase the entries for services it deliberately left
// running, making them unreachable to `forge env down`.
func TestPersistCarriesForwardSurvivingEntries(t *testing.T) {
	requireProcInspection(t)
	dir, projectID, env := testStack(t)

	survivor := spawnMarked(t, projectID, env, "admin-server")

	// The ledger as the previous full run left it.
	prev := newProcRegistry(projectID, dir, env)
	prev.processes = []*managedProcess{
		{name: "admin-server", pid: survivor.pid(), cmd: &exec.Cmd{}},
		{name: "frontend:reliant-web", pid: 999999, cmd: &exec.Cmd{}}, // dead: must be dropped
	}
	prev.persist()

	// The scoped run starts only the frontend, under a new pid.
	scoped := newProcRegistry(projectID, dir, env)
	scoped.processes = []*managedProcess{
		{name: "frontend:reliant-web", pid: survivor.pid(), cmd: &exec.Cmd{}},
	}
	scoped.persist()

	entries := trackedStack(projectID, env)
	byName := map[string]int{}
	for _, e := range entries {
		byName[e.name] = e.pid
	}
	if byName["admin-server"] != survivor.pid() {
		t.Errorf("the surviving service was dropped from the ledger: %+v", entries)
	}
	if byName["frontend:reliant-web"] != survivor.pid() {
		t.Errorf("the restarted service did not take the new pid: %+v", entries)
	}
	if len(entries) != 2 {
		t.Errorf("expected exactly the survivor + the restarted service, got %+v", entries)
	}
}
