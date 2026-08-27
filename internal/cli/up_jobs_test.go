package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The rendered KCL contract for a project that declares a
// forge.OneShotJob must unmarshal into the Jobs bucket the host runner
// reads. This is the KCL→Go seam; the literal below is the actual output
// of `kcl run deploy/kcl/dev -S output` for a scaffolded project with a
// job declared.
func TestKCLEntities_ParsesJobsBucket(t *testing.T) {
	raw := `{"services":[{"name":"item","deploy":{"type":"host"}}],
	         "jobs":[{"name":"provision-idp","image":"jobdemo",
	                  "command":["sh","-c","echo PROVISIONED"],
	                  "before":["item"],"timeout_seconds":0,"env_vars":[]}]}`
	var e KCLEntities
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(e.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(e.Jobs))
	}
	j := e.Jobs[0]
	if j.Name != "provision-idp" || len(j.Command) != 3 || j.Before[0] != "item" {
		t.Errorf("job parsed wrong: %+v", j)
	}
	if err := validateJobOrdering(e.Jobs, e.Services); err != nil {
		t.Errorf("real rendered contract rejected: %v", err)
	}
}

// An env rendered before the jobs bucket existed must still load — the
// bucket is additive, not a new requirement.
func TestKCLEntities_NoJobsBucketIsFine(t *testing.T) {
	var e KCLEntities
	if err := json.Unmarshal([]byte(`{"services":[{"name":"item"}]}`), &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(e.Jobs) != 0 {
		t.Errorf("jobs = %d, want 0", len(e.Jobs))
	}
}

func TestJobsGating(t *testing.T) {
	jobs := []JobEntity{
		{Name: "provision", Before: []string{"api", "sync"}},
		{Name: "seed", Before: nil},
		{Name: "warm", Before: []string{"api"}},
	}
	if got := jobsGating("api", jobs); len(got) != 2 || got[0] != "provision" || got[1] != "warm" {
		t.Errorf("jobsGating(api) = %v, want [provision warm]", got)
	}
	if got := jobsGating("sync", jobs); len(got) != 1 || got[0] != "provision" {
		t.Errorf("jobsGating(sync) = %v, want [provision]", got)
	}
	if got := jobsGating("nobody", jobs); len(got) != 0 {
		t.Errorf("jobsGating(nobody) = %v, want empty", got)
	}
}

// A `before` naming a component that does not exist must be rejected
// with the name involved. Silently ignoring it is the failure this guard
// exists to prevent: the job runs, nothing waits, and the dependent
// fails later somewhere that names none of this.
func TestValidateJobOrdering_DanglingBefore(t *testing.T) {
	jobs := []JobEntity{{Name: "provision", Before: []string{"apiserver"}}}
	services := []ServiceEntity{{Name: "api"}}

	err := validateJobOrdering(jobs, services)
	if err == nil {
		t.Fatal("expected an error for a before naming a nonexistent component")
	}
	for _, want := range []string{"provision -> apiserver", "api", "does not exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}

func TestValidateJobOrdering_Cycle(t *testing.T) {
	jobs := []JobEntity{
		{Name: "a", Before: []string{"b"}},
		{Name: "b", Before: []string{"a"}},
	}
	err := validateJobOrdering(jobs, nil)
	if err == nil {
		t.Fatal("expected an error for a job-gates-job cycle")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should name the cycle:\n%s", err)
	}
}

// A job gating a SERVICE (not another job) is the ordinary case and must
// not be mistaken for a cycle.
func TestValidateJobOrdering_Valid(t *testing.T) {
	jobs := []JobEntity{
		{Name: "provision", Before: []string{"api"}},
		{Name: "seed", Before: []string{"api", "provision"}},
	}
	services := []ServiceEntity{{Name: "api"}}
	if err := validateJobOrdering(jobs, services); err != nil {
		t.Fatalf("valid ordering rejected: %v", err)
	}
}

// A job that gates another job must run first, regardless of the order
// the two were declared in.
func TestOrderJobs_JobGatesJob(t *testing.T) {
	jobs := []JobEntity{
		{Name: "seed", Before: []string{"api"}},
		{Name: "provision", Before: []string{"seed"}},
	}
	got := orderJobs(jobs)
	if got[0].Name != "provision" || got[1].Name != "seed" {
		t.Errorf("orderJobs = [%s %s], want [provision seed]", got[0].Name, got[1].Name)
	}
}

// Independent jobs keep declaration order — the common case must not be
// reshuffled by the sort.
func TestOrderJobs_StableForIndependentJobs(t *testing.T) {
	jobs := []JobEntity{
		{Name: "one", Before: []string{"api"}},
		{Name: "two", Before: []string{"api"}},
		{Name: "three", Before: []string{"api"}},
	}
	got := orderJobs(jobs)
	for i, want := range []string{"one", "two", "three"} {
		if got[i].Name != want {
			t.Errorf("orderJobs[%d] = %s, want %s", i, got[i].Name, want)
		}
	}
}

// The whole point of the primitive: a job that exits non-zero must STOP
// the up, naming what it gated, rather than letting the dependents run.
func TestRunOneHostJob_FailureIsFailClosed(t *testing.T) {
	job := JobEntity{
		Name:    "provision",
		Command: []string{"sh", "-c", "echo provisioning failed >&2; exit 3"},
		Before:  []string{"api"},
	}
	err := runOneHostJob(context.Background(), nil, job, nil, "")
	if err == nil {
		t.Fatal("expected a failing job to return an error")
	}
	for _, want := range []string{"provision", "api", "none of them were started"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}

func TestRunOneHostJob_SuccessRunsToCompletion(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	job := JobEntity{
		Name:    "provision",
		Command: []string{"sh", "-c", "echo done > " + marker},
	}
	if err := runOneHostJob(context.Background(), nil, job, nil, ""); err != nil {
		t.Fatalf("job failed: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("job did not actually run: %v", err)
	}
}

// A one-shot that never exits must fail loudly instead of hanging the
// up forever.
func TestRunOneHostJob_TimeoutIsReported(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a subprocess that sleeps; covered in full mode")
	}
	job := JobEntity{
		Name:           "hangs",
		Command:        []string{"sh", "-c", "sleep 30"},
		Before:         []string{"api"},
		TimeoutSeconds: 1,
	}
	start := time.Now()
	err := runOneHostJob(context.Background(), nil, job, nil, "")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("timeout took %s; the ceiling did not fire", elapsed)
	}
	if !strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("error should explain the timeout:\n%s", err)
	}
}

// The job's own env_vars reach the process.
func TestRunOneHostJob_EnvVarsReachTheProcess(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "env")
	job := JobEntity{
		Name:    "provision",
		Command: []string{"sh", "-c", "echo $IDP_URL > " + marker},
		EnvVars: []KCLEnvVar{{Name: "IDP_URL", Value: "http://idp:8080"}},
	}
	if err := runOneHostJob(context.Background(), nil, job, nil, ""); err != nil {
		t.Fatalf("job failed: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if strings.TrimSpace(string(got)) != "http://idp:8080" {
		t.Errorf("IDP_URL = %q, want http://idp:8080", strings.TrimSpace(string(got)))
	}
}

// An env with no jobs must behave exactly as it did before the bucket
// existed.
func TestRunHostJobs_NoJobsIsANoop(t *testing.T) {
	if err := runHostJobs(context.Background(), nil, &KCLEntities{}, nil, "dev"); err != nil {
		t.Fatalf("empty job set errored: %v", err)
	}
}

// Ordering across a multi-job sequence, observed by the jobs themselves.
func TestRunHostJobs_RunsInDependencyOrder(t *testing.T) {
	log := filepath.Join(t.TempDir(), "order")
	e := &KCLEntities{
		Services: []ServiceEntity{{Name: "api"}},
		Jobs: []JobEntity{
			// Declared second-first on purpose: `seed` is gated by
			// `provision`, so provision must still run first.
			{Name: "seed", Command: []string{"sh", "-c", "echo seed >> " + log}, Before: []string{"api"}},
			{Name: "provision", Command: []string{"sh", "-c", "echo provision >> " + log}, Before: []string{"seed"}},
		},
	}
	if err := runHostJobs(context.Background(), nil, e, nil, ""); err != nil {
		t.Fatalf("jobs failed: %v", err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read order log: %v", err)
	}
	want := "provision\nseed\n"
	if string(got) != want {
		t.Errorf("execution order = %q, want %q", got, want)
	}
}

// A failing job stops the sequence: the jobs after it never run.
func TestRunHostJobs_StopsAtFirstFailure(t *testing.T) {
	log := filepath.Join(t.TempDir(), "order")
	e := &KCLEntities{
		Services: []ServiceEntity{{Name: "api"}},
		Jobs: []JobEntity{
			{Name: "broken", Command: []string{"sh", "-c", "exit 1"}, Before: []string{"api"}},
			{Name: "later", Command: []string{"sh", "-c", "echo later >> " + log}, Before: []string{"api"}},
		},
	}
	if err := runHostJobs(context.Background(), nil, e, nil, ""); err == nil {
		t.Fatal("expected the failing job to stop the sequence")
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("a job after the failing one ran; the sequence did not stop")
	}
}

// ---------------------------------------------------------------------------
// The BROADCAST selector on the host lowering.
// ---------------------------------------------------------------------------
//
// `before = ["*"]` is how the deploy-time migration step is declared, so
// these are the host-side no-regression tests for it. The k8s and compose
// lowerings expand the wildcard in KCL; the host runner has to do it in Go,
// because there is no orchestrator underneath it to enforce the ordering.

// The wildcard is a SELECTOR, not a name. Treating it as a name is the
// bug this pins: validateJobOrdering rejected `*` as a dangling
// reference, which failed every `forge run` of a project that scaffolds
// a migration — with a message telling the reader to fix a name that was
// never wrong.
func TestValidateJobOrdering_BroadcastIsNotADanglingName(t *testing.T) {
	jobs := []JobEntity{
		{Name: "migrate", Command: []string{"true"}, Before: []string{BeforeAll}},
	}
	svcs := []ServiceEntity{{Name: "api"}, {Name: "sync"}}
	if err := validateJobOrdering(jobs, svcs); err != nil {
		t.Fatalf("broadcast before rejected: %v", err)
	}
}

// A broadcast job gates every service without naming one — including a
// service added after it was written, which is the entire reason the
// selector exists.
func TestJobsGating_BroadcastGatesEveryServiceAndNeverItself(t *testing.T) {
	jobs := []JobEntity{
		{Name: "migrate", Command: []string{"true"}, Before: []string{BeforeAll}},
		{Name: "seed", Command: []string{"true"}},
	}
	for _, svc := range []string{"api", "sync", "a-service-added-later"} {
		got := jobsGating(svc, jobs)
		if len(got) != 1 || got[0] != "migrate" {
			t.Errorf("jobsGating(%q) = %v, want [migrate]", svc, got)
		}
	}
	// A broadcast job gates other JOBS too — a seed must not beat the
	// migration that creates the table it seeds.
	if got := jobsGating("seed", jobs); len(got) != 1 || got[0] != "migrate" {
		t.Errorf("jobsGating(seed) = %v, want [migrate]", got)
	}
	// ...but never itself: a job cannot wait for its own completion.
	if got := jobsGating("migrate", jobs); len(got) != 0 {
		t.Errorf("jobsGating(migrate) = %v, want []", got)
	}
}

// Two broadcast jobs are PEERS, not a cycle. Each gates everything else;
// neither gates the other, so the graph stays acyclic by construction
// rather than by a cycle check catching it.
func TestValidateJobOrdering_TwoBroadcastJobsAreNotACycle(t *testing.T) {
	jobs := []JobEntity{
		{Name: "migrate", Command: []string{"true"}, Before: []string{BeforeAll}},
		{Name: "provision", Command: []string{"true"}, Before: []string{BeforeAll}},
	}
	if err := validateJobOrdering(jobs, []ServiceEntity{{Name: "api"}}); err != nil {
		t.Fatalf("two broadcast jobs reported as invalid: %v", err)
	}
}

// A broadcast job RUNS FIRST, even when it is declared last. Ordering by
// the (empty) name list would score it zero and run the migration after
// the seed job it is supposed to precede.
func TestRunHostJobs_BroadcastRunsBeforeOtherJobs(t *testing.T) {
	log := filepath.Join(t.TempDir(), "order")
	e := &KCLEntities{
		Services: []ServiceEntity{{Name: "api"}},
		Jobs: []JobEntity{
			{Name: "seed", Command: []string{"sh", "-c", "echo seed >> " + log}, Before: []string{"api"}},
			{Name: "migrate", Command: []string{"sh", "-c", "echo migrate >> " + log}, Before: []string{BeforeAll}},
		},
	}
	if err := runHostJobs(context.Background(), nil, e, nil, ""); err != nil {
		t.Fatalf("jobs failed: %v", err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read order log: %v", err)
	}
	if want := "migrate\nseed\n"; string(got) != want {
		t.Errorf("execution order = %q, want %q", got, want)
	}
}

// A job's argv is written for the CONTAINER (`/app/<project> db migrate
// up`). That path does not exist on a developer's laptop, so the host
// runner rewrites argv[0] to the go-run target — the same translation
// the host launcher makes for a service's command. Without it, every
// `forge run` of a project that scaffolds a migration dies with "no such
// file or directory" naming a path the reader never wrote.
func TestCollapseJobsToHost_RewritesInImageArgv(t *testing.T) {
	e := &KCLEntities{Jobs: []JobEntity{
		{Name: "migrate", Command: []string{"/app/demo", "db", "migrate", "up"}, Before: []string{BeforeAll}},
		// Not the project binary — deliberately wired by the author, so
		// forge must leave it exactly as written.
		{Name: "custom", Command: []string{"/usr/local/bin/psql", "-f", "seed.sql"}},
	}}
	collapseJobsToHost(e, "demo")

	want := []string{"go", "run", "./cmd/demo", "db", "migrate", "up"}
	if got := e.Jobs[0].Command; !slicesEqual(got, want) {
		t.Errorf("migrate argv = %v, want %v", got, want)
	}
	wantUntouched := []string{"/usr/local/bin/psql", "-f", "seed.sql"}
	if got := e.Jobs[1].Command; !slicesEqual(got, wantUntouched) {
		t.Errorf("non-project argv was rewritten: %v, want %v", got, wantUntouched)
	}
}

// Each rewritten job must get its own backing array. The go-run prefix is
// built once per job, so a shared array would let the longer job's tail
// overwrite the shorter one's — the second job silently inheriting the
// first's subcommand, which is the failure mode that reads as "forge ran
// the wrong migration".
func TestCollapseJobsToHost_RewrittenJobsDoNotShareBacking(t *testing.T) {
	e := &KCLEntities{Jobs: []JobEntity{
		{Name: "migrate", Command: []string{"/app/demo", "db", "migrate", "up"}},
		{Name: "seed", Command: []string{"/app/demo", "db", "seed"}},
	}}
	collapseJobsToHost(e, "demo")

	wantMigrate := []string{"go", "run", "./cmd/demo", "db", "migrate", "up"}
	if got := e.Jobs[0].Command; !slicesEqual(got, wantMigrate) {
		t.Errorf("migrate argv = %v, want %v", got, wantMigrate)
	}
	wantSeed := []string{"go", "run", "./cmd/demo", "db", "seed"}
	if got := e.Jobs[1].Command; !slicesEqual(got, wantSeed) {
		t.Errorf("seed argv = %v, want %v", got, wantSeed)
	}

	// Mutating one job's argv must not be observable in the other.
	e.Jobs[1].Command[len(e.Jobs[1].Command)-1] = "CLOBBERED"
	if got := e.Jobs[0].Command; !slicesEqual(got, wantMigrate) {
		t.Errorf("writing to seed argv changed migrate argv: %v, want %v", got, wantMigrate)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
