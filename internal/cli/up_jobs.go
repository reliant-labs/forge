package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/hostlaunch"
)

// The HOST lowering of the one-shot `job` component kind.
//
// k8s and compose each have a native way to say "run this to completion,
// then start that": an init container, and a `depends_on` with
// `condition: service_completed_successfully`. The host runner has
// neither, because there is no orchestrator underneath it — it is a
// process launcher. So the ordering the other two targets DELEGATE, this
// one has to ENFORCE: run the argv, wait for exit 0, and only then let
// the gated services launch.
//
// That asymmetry is the whole reason `Job` is a primitive rather than a
// k8s feature with a compose shim. The declaration is identical across
// the three; only the enforcement mechanism differs.

// defaultJobTimeout bounds the wait for a one-shot that declares no
// timeout of its own.
//
// A one-shot that never exits is indistinguishable, from the outside,
// from one that is merely slow — and the failure mode of guessing wrong
// in the permissive direction is `forge env up` hanging forever with no
// output, which is the worst possible signal. Five minutes is far longer
// than any provisioning step should take and short enough that a wedged
// job is noticed within one coffee.
const defaultJobTimeout = 5 * time.Minute

// BeforeAll is the BROADCAST selector for a one-shot's `before`: "gate
// every workload in this environment", as opposed to an enumerated list
// of dependent names. It mirrors `forge.workloads.BEFORE_ALL` in the KCL
// layer, which is where the value is defined and documented.
//
// A workload can never be NAMED this — a workload name is used verbatim
// as a Kubernetes object name and "*" is not a legal DNS-1123 label — so
// the wildcard cannot collide with a real dependent.
const BeforeAll = "*"

// isBroadcast reports whether a one-shot gates everything rather than an
// enumerated set.
func (j JobEntity) isBroadcast() bool {
	for _, dep := range j.Before {
		if dep == BeforeAll {
			return true
		}
	}
	return false
}

// jobsGating returns the names of the one-shot jobs that must complete
// before the named service launches, in declaration order.
//
// A BROADCAST job gates every service, so it is returned for all of
// them without appearing in any list — which is the entire point: the
// service added next month is gated with nothing to update.
func jobsGating(service string, jobs []JobEntity) []string {
	var out []string
	for _, j := range jobs {
		if j.Name == service {
			continue
		}
		if j.isBroadcast() {
			out = append(out, j.Name)
			continue
		}
		for _, dep := range j.Before {
			if dep == service {
				out = append(out, j.Name)
				break
			}
		}
	}
	return out
}

// validateJobOrdering rejects a job graph that cannot be executed as
// declared, BEFORE any process starts.
//
// Two failures are possible and both are silent otherwise:
//
//   - `before` naming something that does not exist. The job runs, nothing
//     waits for it, and the ordering the author declared just does not
//     happen. The service then fails somewhere else entirely, at runtime,
//     in a message that names none of this.
//   - a cycle. Two jobs each waiting for the other deadlocks the up.
//
// Both are reported with the names involved, because "invalid job graph"
// without the names is a message that sends the reader back to the file
// to work out what forge already knew.
func validateJobOrdering(jobs []JobEntity, services []ServiceEntity) error {
	known := map[string]bool{}
	for _, s := range services {
		known[s.Name] = true
	}
	for _, j := range jobs {
		known[j.Name] = true
	}

	var dangling []string
	for _, j := range jobs {
		for _, dep := range j.Before {
			// The BROADCAST selector is not a name and cannot dangle —
			// there is no list to go stale, which is why it exists.
			if dep == BeforeAll {
				continue
			}
			if !known[dep] {
				dangling = append(dangling, fmt.Sprintf("%s -> %s", j.Name, dep))
			}
		}
	}
	if len(dangling) > 0 {
		names := make([]string, 0, len(known))
		for n := range known {
			names = append(names, n)
		}
		sort.Strings(names)
		return fmt.Errorf(
			"job ordering names a component that does not exist: %s\n"+
				"  declared components: %s\n"+
				"  fix the name in deploy/kcl/<env>/main.k, or drop `before` to run the job without gating anything",
			strings.Join(dangling, ", "), strings.Join(names, ", "))
	}

	// A job may gate another job; that is a legitimate sequence
	// (provision, then seed). A CYCLE in that graph is not.
	edges := map[string][]string{}
	isJob := map[string]bool{}
	broadcast := map[string]bool{}
	for _, j := range jobs {
		isJob[j.Name] = true
		broadcast[j.Name] = j.isBroadcast()
	}
	for _, j := range jobs {
		if j.isBroadcast() {
			// A broadcast job gates every OTHER job, but never another
			// broadcast job: two of them would each claim to precede the
			// other, which is not an ordering. They are peers — both run
			// before everything else, in declaration order — so no edge
			// is drawn between them and the graph cannot cycle through
			// the wildcard.
			for _, other := range jobs {
				if other.Name != j.Name && !broadcast[other.Name] {
					edges[j.Name] = append(edges[j.Name], other.Name)
				}
			}
			continue
		}
		for _, dep := range j.Before {
			if isJob[dep] {
				edges[j.Name] = append(edges[j.Name], dep)
			}
		}
	}
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var visit func(string) error
	visit = func(n string) error {
		color[n] = grey
		stack = append(stack, n)
		for _, m := range edges[n] {
			switch color[m] {
			case grey:
				return fmt.Errorf(
					"job ordering has a cycle: %s -> %s\n"+
						"  a job cannot wait for a job that waits for it; break the cycle in deploy/kcl/<env>/main.k",
					strings.Join(stack, " -> "), m)
			case white:
				if err := visit(m); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return nil
	}
	names := make([]string, 0, len(isJob))
	for n := range isJob {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if color[n] == white {
			if err := visit(n); err != nil {
				return err
			}
		}
	}
	return nil
}

// orderJobs returns the jobs in an execution order that respects
// job-gates-job edges (a job listed in another job's `before` runs
// first). Ties keep declaration order, so the common case — a flat list
// of independent one-shots — runs exactly as written.
func orderJobs(jobs []JobEntity) []JobEntity {
	pos := map[string]int{}
	for i, j := range jobs {
		pos[j.Name] = i
	}
	broadcast := map[string]bool{}
	for _, j := range jobs {
		broadcast[j.Name] = j.isBroadcast()
	}
	// depth = how many job-gates-job edges lead OUT of this job. A job
	// that gates another must run before it, so higher depth sorts first.
	depth := map[string]int{}
	var compute func(string, map[string]bool) int
	compute = func(n string, seen map[string]bool) int {
		if d, ok := depth[n]; ok {
			return d
		}
		if seen[n] {
			return 0 // cycle; validateJobOrdering rejects it separately
		}
		seen[n] = true
		best := 0
		for _, j := range jobs {
			if j.Name != n {
				continue
			}
			// A broadcast job gates every other non-broadcast job, so its
			// depth is one deeper than the deepest of them. Computing it
			// from the (absent) name list would score it 0 and run the
			// migration AFTER the seed job it is supposed to precede.
			if j.isBroadcast() {
				for _, other := range jobs {
					if other.Name == n || broadcast[other.Name] {
						continue
					}
					if d := compute(other.Name, seen) + 1; d > best {
						best = d
					}
				}
				continue
			}
			for _, dep := range j.Before {
				if _, isJob := pos[dep]; !isJob {
					continue
				}
				if d := compute(dep, seen) + 1; d > best {
					best = d
				}
			}
		}
		depth[n] = best
		return best
	}
	for _, j := range jobs {
		compute(j.Name, map[string]bool{})
	}
	out := append([]JobEntity(nil), jobs...)
	sort.SliceStable(out, func(a, b int) bool {
		if depth[out[a].Name] != depth[out[b].Name] {
			return depth[out[a].Name] > depth[out[b].Name]
		}
		return pos[out[a].Name] < pos[out[b].Name]
	})
	return out
}

// runHostJobs runs every one-shot job to completion, in dependency
// order, before the host service phase launches anything.
//
// It is deliberately FAIL-CLOSED: a job that exits non-zero (or times
// out) stops the up. The entire premise of the primitive is that the
// dependents must not run against a world the job was supposed to have
// prepared — proceeding anyway would turn a clear, local failure into an
// obscure one inside a service that has no idea why its dependency is
// missing.
//
// cfg / env feed the same projectConfig env layer host services get, so
// a job and the service it gates see the same configuration.
func runHostJobs(ctx context.Context, cfg *config.ProjectConfig, e *KCLEntities, secretsLayer map[string]string, env string) error {
	if len(e.Jobs) == 0 {
		return nil
	}
	if err := validateJobOrdering(e.Jobs, e.Services); err != nil {
		return err
	}
	for _, j := range orderJobs(e.Jobs) {
		err := runOneHostJob(ctx, cfg, j, secretsLayer, env)
		if err == nil {
			continue
		}
		// Fail-closed is about the DEPENDENTS. A job that gates nothing has
		// no dependents to protect, so aborting the whole up on its failure
		// takes down a stack it was not standing in front of — which is what
		// happened to every second project on a machine: the dev IdP's port
		// was already held by another stack, idp-provision could not
		// converge, and `forge run` refused to start the app at all. The
		// backend and frontend would have come up perfectly well; only
		// sign-in was unavailable.
		//
		// So a gating job still stops the up, and a non-gating one degrades:
		// loudly, naming what is now missing, and continuing.
		if len(j.Before) > 0 || j.isBroadcast() {
			return err
		}
		fmt.Printf("[up] job %s FAILED — continuing, because it gates nothing:\n%v\n", j.Name, err)
		fmt.Printf("[up] whatever %s provisions is unavailable this run; everything else still starts.\n", j.Name)
	}
	return nil
}

// runOneHostJob runs a single one-shot to completion and reports whether
// it exited 0.
func runOneHostJob(ctx context.Context, cfg *config.ProjectConfig, j JobEntity, secretsLayer map[string]string, env string) error {
	if len(j.Command) == 0 {
		return fmt.Errorf("job %s: no command declared — a one-shot with no command has nothing to run to completion", j.Name)
	}
	timeout := defaultJobTimeout
	if j.TimeoutSeconds > 0 {
		timeout = time.Duration(j.TimeoutSeconds) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, j.Command[0], j.Command[1:]...) // #nosec G204 -- argv is declared in the project's own KCL
	cmd.Dir = projectDirForKCL()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Same env composition host services get: projectConfig → secrets →
	// the job's own env_vars → os.Environ() wins last. A job that
	// provisions the thing a service dials must see the same coordinates
	// that service will.
	var projectConfigEnv map[string]string
	if cfg != nil && env != "" {
		projectConfigEnv = loadProjectConfigEnv(cfg, env)
	}
	// Same dev-run defaults host services get (development runtime,
	// AUTO_MIGRATE on a dev env). A provisioning job that dials the
	// database must see the same DSN the service will.
	dev, _ := seedTargetIsDev(env)
	projectConfigEnv = withDevRunDefaults(projectConfigEnv, dev)
	// A job's OWN KCL env_vars are the last of the three map layers, so a
	// value the environment DECLARES for this job beats project config and
	// secrets alike. That is how the dev IdP's address reaches
	// idp-provision: deploy/kcl/dev/main.k hands it IDP_BASE /
	// IDP_BROWSER_ORIGIN composed from the same port it publishes the
	// container on, so the job dials the IdP that is actually running
	// rather than the "http://localhost:8080" literal baked into its flag
	// defaults at project-generation time.
	cmd.Env = hostlaunch.LayerHostEnv(os.Environ(), projectConfigEnv, secretsLayer, kclEnvVarsToMap(j.EnvVars))

	// What this job gates, for the human reading the log. The raw
	// `before` would print a bare "*", which says nothing about what is
	// actually waiting; spell the selector out instead.
	gated := strings.Join(j.Before, ", ")
	switch {
	case j.isBroadcast():
		gated = "every workload in this environment"
	case gated == "":
		gated = "nothing"
	}
	fmt.Printf("[up] job %s: running to completion (gates: %s)\n", j.Name, gated)

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start).Round(time.Millisecond)
	switch {
	case err == nil:
		fmt.Printf("[up] job %s: completed in %s\n", j.Name, elapsed)
		return nil
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return fmt.Errorf(
			"job %s did not finish within %s and was killed\n"+
				"  it gates: %s\n"+
				"  a one-shot must RUN TO COMPLETION; if this job is legitimately slow, raise its timeout_seconds in deploy/kcl/<env>/main.k",
			j.Name, timeout, gated)
	default:
		return fmt.Errorf(
			"job %s failed after %s: %w\n"+
				"  it gates: %s — none of them were started\n"+
				"  fix the job (or its configuration) and re-run; forge will not start a dependent against a world the job was supposed to prepare",
			j.Name, elapsed, err, gated)
	}
}
