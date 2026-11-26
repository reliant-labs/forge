// Package cli — `forge env restart <env> <workload>...`: replace one process
// of a running local stack.
//
// A host process can stop while the stack around it keeps running: it
// crashed, someone sent it SIGTERM, or Air's server died while Air lived on
// (Air never relaunches a server that exits on its own, and ignores a `touch`).
// The stack then has no verb that brings back just that process. `forge env up`
// replaces the whole stack, and `forge env exec` runs a command beside it that
// nothing tracks.
//
// A restart is the scoped `forge env up --target` replace, run through the SAME
// pipeline, so the new process is launched with exactly what `env up` gives it:
// the env's rendered configuration, its secrets, its ports. What a restart
// leaves out is everything that converges the env around the process: no image
// build, no cluster apply, no one-shot jobs, no seed, no new presence row. It
// always detaches. The process it stops is selected by forge's ownership
// markers for THIS project and env, as every teardown is, so a process it
// cannot prove is the workload's is never signalled; a process that is already
// dead is simply started again.
//
// Three things keep the restarted process part of the stack it joins:
//
//   - It keeps the ports the stack runs on (reuseLivePorts): a workload with no
//     declared port binds the one the stack recorded, not a fresh ephemeral
//     one, so the frontends and anything else wired to it still reach it.
//   - It is recorded beside the processes left running (procRegistry.persist
//     merges the ledger; mergeUntargetedPorts merges the port record), so
//     `forge env status`, `forge env ps` and `forge env down` all see it.
//   - It carries the run marker of the stack it joins (upRestart.runID). A
//     supervising `forge env up` in another terminal therefore keeps holding
//     while it lives and stops it on Ctrl-C, like the process it replaced; the
//     restart's lease (below) keeps that supervisor from ending its hold in the
//     moment between the old process stopping and the new one starting.
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"github.com/reliant-labs/forge/internal/config"
)

const envRestartLongHelp = `Restart host processes or frontends of a RUNNING local stack.

Stops each named workload's process tree, if it is still running, and starts
it again with the configuration ` + "`forge env up`" + ` gives it: the env's rendered
config, its secrets, and the ports the stack is already on. It works whether
the process is alive or has already exited (crashed, killed, or an Air server
that died while Air lived on), and every other process of the stack keeps
running.

Only what this project's ` + "`forge env up`" + ` started for this env is ever stopped:
the process is found by forge's ownership markers, and nothing else is
signalled. A stopping process gets the stack's recorded stop grace (the
workload's preStopSeconds, or Air's kill_delay) before SIGKILL.

A restart rebuilds nothing itself: air and go-run compile when they start, a
binary runner restarts the binary ` + "`forge build`" + ` last made. It runs no image
build, cluster apply, one-shot job or seed — ` + "`forge env up <env> --target <name>`" + `
does all of that for one workload. The restarted process is detached, with its
output in its log file (see the summary); ` + "`forge env status <env>`" + ` reports it and
` + "`forge env down <env>`" + ` stops it. A supervising ` + "`forge env up`" + ` in another terminal
keeps owning it, and stops it on Ctrl-C.

The stack must be running: a stack ` + "`forge env up`" + ` never started, or one
` + "`forge env down`" + ` stopped, is refused (start it with ` + "`forge env up <env>`" + `).
Cluster and Compose workloads are not processes forge supervises; for those,
` + "`forge env up <env> --target <name>`" + ` re-converges one.

Examples:
  forge env restart dev api
  forge env restart dev api web`

func newEnvRestartCmd() *cobra.Command {
	var readyTimeout time.Duration
	cmd := &cobra.Command{
		Use:          "restart <environment> <workload> [<workload>...]",
		Short:        "Restart host processes or frontends of a running local stack, keeping its ports, config and secrets",
		Long:         envRestartLongHelp,
		Args:         cobra.MinimumNArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if readyTimeout <= 0 {
				return fmt.Errorf("--host-ready-timeout must be positive")
			}
			return runEnvRestart(cmd, args[0], args[1:], readyTimeout)
		},
	}
	cmd.Flags().DurationVar(&readyTimeout, "host-ready-timeout", hostReadyTimeout, "Maximum wait for a restarted host service to bind its ports, including compilation; an exited runner fails immediately")
	return cmd
}

func runEnvRestart(cmd *cobra.Command, env string, targets []string, readyTimeout time.Duration) error {
	projectDir, err := requireStackProjectDir("env restart")
	if err != nil {
		return err
	}
	projectID := projectIDForDir(projectDir)
	recorded := loadResolvedEnv(projectID, env)
	facts := newOSProcFacts()
	stack := inspectRestartStack(projectID, env, targets, facts.pidList(), facts,
		recorded != nil || len(trackedStack(projectID, env)) > 0)
	if !stack.running {
		return fmt.Errorf("env=%s is not running in this project: no process of its stack is alive and no launch is recorded — "+
			"`forge env restart` replaces processes of a running stack; bring it up with `forge env up %s`", env, env)
	}
	release, err := takeRestartLease(projectID, env)
	if err != nil {
		return err
	}
	defer release()

	opts := upOptions{
		env:     env,
		targets: targets,
		// A restart never holds the terminal: the stack it joins is
		// supervised by whoever started it, or detached.
		background: true,
		// Nothing around the process is re-converged: no image build or
		// cluster apply, and no first-boot seed.
		noBuild: true, noDeploy: true, noSeed: true,
		hostReadyTimeout: readyTimeout,
		restart:          &upRestart{runID: stack.runID},
	}
	if recorded != nil {
		// The -D options the stack was brought up with, so the restarted
		// process renders the configuration its siblings run with.
		opts.renderOptions = recorded.RenderOptions
	}
	return runUp(cmd.Context(), opts)
}

// upRestart marks a runUp as `forge env restart`. nil for every `forge env up`.
type upRestart struct {
	// runID is the FORGE_UP_RUN of the stack being restarted into, stamped on
	// the new process so the run that owns the stack still owns it. Empty
	// when no live process of the stack carries one: the restart then runs
	// under a fresh id, like any detached run.
	runID string
}

// runsJobs reports whether this run runs the env's one-shot jobs: every
// `forge env up` does; a restart replaces a process and prepares nothing.
func (r *upRestart) runsJobs() bool { return r == nil }

// joinsSession reports whether this run reports the stack's presence and
// starts its telemetry keeper. A restart joins a stack that already did.
func (r *upRestart) joinsSession() bool { return r == nil }

// checkTargets refuses, before anything is stopped, a restart of something
// that is not a process `forge env up` supervises on this machine: a cluster
// or Compose workload, a job, a build-only variant. A nil receiver (a plain
// `forge env up`) accepts everything.
func (r *upRestart) checkTargets(e *KCLEntities, env string, targets []string) error {
	if r == nil || e == nil {
		return nil
	}
	var problems []string
	for _, name := range targets {
		if why := restartRefusal(e, env, name); why != "" {
			problems = append(problems, why)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("nothing was stopped: %s (forge env restart restarts only the host processes and frontends `forge env up` supervises)",
		strings.Join(problems, "; "))
}

// restartRefusal is why name cannot be restarted, or "" when it can.
func restartRefusal(e *KCLEntities, env, name string) string {
	for _, fe := range e.Frontends {
		if fe.Name == name {
			return ""
		}
	}
	w := e.FindWorkload(name)
	if w == nil {
		// validateDeployTargets already refused an unknown name; anything
		// else that reaches here (an infra service) is no supervised process.
		return fmt.Sprintf("%s is not a workload of env %s; `forge env up %s` converges the env's infrastructure", name, env, env)
	}
	switch {
	case w.OnRuntime(RuntimeHost) && w.LongRunning():
		return ""
	case w.OnRuntime(RuntimeHost):
		return fmt.Sprintf("%s is a %s: it runs to completion during `forge env up %s`, so there is no process to restart", name, emptyAs(w.Kind, "job"), env)
	default:
		return fmt.Sprintf("%s runs on the %s runtime, not as a host process: `forge env up %s --target %s` re-converges it",
			name, emptyAs(string(w.Runtime.Type), "unknown"), env, name)
	}
}

// restartStack is what the process table and the records say about the stack
// a restart is about to join.
type restartStack struct {
	// running: a live process carries the stack's markers, or a launch is
	// recorded that no `forge env down` removed.
	running bool
	// runID is the run marker to stamp on the restarted process: the one the
	// process being replaced carries, else the one another live process of
	// the stack carries (lowest pid, so the choice is stable), else "".
	runID string
}

// inspectRestartStack reads the stack's live processes from facts. recorded
// says whether the stack has a launch record; a stack whose only process died
// is still running in that sense — that is the restart this command is for.
// Pure over the injected facts.
func inspectRestartStack(projectID, env string, targets []string, pids []int, f procFacts, recorded bool) restartStack {
	sorted := append([]int(nil), pids...)
	sort.Ints(sorted)
	st := restartStack{running: recorded}
	var targetRun, otherRun string
	for _, pid := range sorted {
		if pid <= 1 {
			continue
		}
		environ, ok := f.environ(pid)
		if !ok {
			continue
		}
		service, matched := markerMatches(environ, projectID, env)
		if !matched {
			continue
		}
		st.running = true
		run := markerValue(environ, forgeUpRunVar)
		if run == "" {
			continue
		}
		if inTargetSet(targets, strings.TrimPrefix(service, "frontend:")) {
			if targetRun == "" {
				targetRun = run
			}
		} else if otherRun == "" {
			otherRun = run
		}
	}
	st.runID = emptyAs(targetRun, otherRun)
	return st
}

// reuseLivePorts keeps a scoped run on the ports of the stack it runs inside.
// A host workload or frontend that declares no port gets an ephemeral one on
// every `forge env up`; that is right for a whole stack, but a scoped run
// (`--target`, or a restart) replaces some processes and leaves the rest
// running, wired to the ports recorded at their launch — the frontend's API
// URL above all. So each such workload binds its recorded port instead.
// Declared ports are left alone (they are stable by declaration), and a port
// that is no longer free fails the run's port-conflict pre-flight as usual.
func reuseLivePorts(e *KCLEntities, cfg *config.ProjectConfig, st *resolvedEnvState) {
	if e == nil || st == nil || len(st.Ports) == 0 {
		return
	}
	for i := range e.Workloads {
		w := &e.Workloads[i]
		if !w.OnRuntime(RuntimeHost) || !w.LongRunning() {
			continue
		}
		host := w.Runtime.Host
		if host.ListenPorts != nil && len(*host.ListenPorts) == 0 {
			continue // binds nothing, as resolveEphemeralHostPorts honours
		}
		port := st.Ports[w.Name]
		if port <= 0 || w.HostPort() > 0 {
			continue
		}
		host.ListenPorts = &[]int{port}
		if host.LaunchEnv == nil {
			host.LaunchEnv = map[string]string{}
		}
		host.LaunchEnv["PORT"] = fmt.Sprintf("%d", port)
		fmt.Printf("[up] host %s: keeping the stack's port %d\n", w.Name, port)
	}
	for i := range e.Frontends {
		fe := &e.Frontends[i]
		port := st.Ports[fe.Name]
		if fe.Port > 0 || port <= 0 {
			continue
		}
		fe.Port = port
		if cfg != nil {
			for j := range cfg.Frontends {
				if cfg.Frontends[j].Name == fe.Name {
					cfg.Frontends[j].Port = port
				}
			}
		}
		fmt.Printf("[up] frontend %s: keeping the stack's port %d\n", fe.Name, port)
	}
}

// mergeUntargetedPorts adds to ports the recorded ports of every workload a
// scoped run did not start: those processes are still running on them, and
// `forge env status` overlays this record to report and probe them. A full
// run (no targets) replaced the whole stack, so its own rows are the record.
func mergeUntargetedPorts(ports map[string]int, prev *resolvedEnvState, targets []string) {
	if len(targets) == 0 || prev == nil {
		return
	}
	for name, port := range prev.Ports {
		if _, ok := ports[name]; !ok && !inTargetSet(targets, name) && port > 0 {
			ports[name] = port
		}
	}
}

// restartHints are the summary-box lines `forge env status` adds for each host
// process or frontend of a recorded stack that is not listening, naming the
// verb that brings it back. A stack with no launch record (never started, or
// stopped by `forge env down`) gets none: `forge env up` is the answer there,
// and the box already says how to run it.
func restartHints(env string, rows []upServiceRow, recorded *resolvedEnvState) []string {
	if recorded == nil {
		return nil
	}
	var down []string
	for _, r := range rows {
		if (r.Kind == "host" || r.Kind == "frontend") && r.Port > 0 && !r.Listening && r.HeldBy == "" {
			down = append(down, r.Name)
		}
	}
	if len(down) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%s not listening — restart: forge env restart %s %s",
		strings.Join(down, ", "), env, strings.Join(down, " "))}
}

// joinedRunAlive reports whether this run's stack is still alive beyond the
// processes the registry started itself: a `forge env restart` is replacing
// one of them right now (its lease is held), or a process a restart started
// carries this run's marker. A supervising run's hold must not end on either:
// every process IT started can have exited while the stack runs on.
func (p *procRegistry) joinedRunAlive() bool {
	if p == nil || p.runID == "" || p.projectID == "" {
		return false
	}
	if restartLeaseHeld(p.projectID, p.env, time.Now()) {
		return true
	}
	p.mu.Lock()
	managed := make(map[int]bool, len(p.processes))
	for _, mp := range p.processes {
		if pid := procPID(mp); pid > 0 {
			managed[pid] = true
		}
	}
	p.mu.Unlock()
	sw, _, err := p.sweep(managed)
	return err == nil && len(sw.roots) > 0
}

// restartLeaseMaxAge bounds how long a restart's lease is honoured, so a lease
// whose pid the OS recycled after the restart died cannot hold a supervisor
// forever. Far longer than any restart (a first compile included).
const restartLeaseMaxAge = time.Hour

// restartLease is <env>.restart beside the stack's ledger: a `forge env
// restart` in progress.
type restartLease struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

func restartLeasePath(projectID, env string) (string, error) {
	dir, err := upProjectCacheDir(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, env+".restart"), nil
}

// takeRestartLease records this restart for the stack's supervisor, and
// returns the func that removes the lease again.
func takeRestartLease(projectID, env string) (func(), error) {
	path, err := restartLeasePath(projectID, env)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(restartLease{PID: os.Getpid(), StartedAt: time.Now().UTC()})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("record the restart: %w", err)
	}
	if err := atomicfile.WriteFile(path, data, 0o644); err != nil {
		return nil, fmt.Errorf("record the restart: %w", err)
	}
	return func() { _ = os.Remove(path) }, nil
}

// restartLeaseHeld reports whether a restart of the stack is in progress: its
// lease exists, names a live process, and is younger than restartLeaseMaxAge.
func restartLeaseHeld(projectID, env string, now time.Time) bool {
	path, err := restartLeasePath(projectID, env)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var l restartLease
	if err := json.Unmarshal(data, &l); err != nil || l.PID <= 0 {
		return false
	}
	return now.Sub(l.StartedAt) < restartLeaseMaxAge && processAlive(l.PID)
}
