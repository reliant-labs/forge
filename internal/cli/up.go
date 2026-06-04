// Package cli — `forge up --env=<env>` orchestrator.
//
// One command brings the whole loop up:
//
//  1. Render the env's KCL → typed entity set.
//  2. Build phase: docker build (per-platform) and push every cluster
//     service / operator / cronjob image; go build each declared
//     build-only variant binary.
//  3. Deploy phase: kubectl apply the cluster manifests + wait operator
//     rollouts + wait one-shot Jobs.
//  4. Host phase: start every host-mode service as a host process,
//     dispatching on deploy.Host.Runner (go-run / air / binary / delve).
//  5. Frontend phase: start every declared frontend in its path dir.
//  6. Port-forward phase: kubectl port-forward every cluster service
//     with deploy.Cluster.Ports.
//  7. Wait Ctrl-C → cascade cleanup → exit.
//
// Replaces the dev-loop bash script every forge project would otherwise
// hand-write to coordinate build + deploy + run + port-forward.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/hostlaunch"
)

// upOptions bundles flags for `forge up`.
type upOptions struct {
	env         string
	noBuild     bool
	noDeploy    bool
	clusterOnly bool // build + deploy cluster manifests, skip host/frontend/port-forward
	hostOnly    bool // skip cluster build+deploy, run host + frontend phases only
	background  bool // detach and write PID files; use `forge up stop --env=<env>` to teardown
}

func newUpCmd() *cobra.Command {
	var opts upOptions

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Bring the whole dev loop up: build + deploy + host + frontend + port-forward",
		Long: `Bring the whole dev loop up for an environment.

Reads deploy/kcl/<env>/ to figure out which services run in-cluster vs
on the host, which frontends to start, and which port-forwards to open.

Phases:
  1. build:        docker build + push every cluster image; go build
                   each build-only variant
  2. deploy:       kubectl apply cluster manifests; wait rollouts and
                   one-shot Jobs
  3. host:         start every host-mode service (go-run / air / binary
                   / delve)
  4. frontend:     start every declared frontend in its path
  5. port-forward: kubectl port-forward every cluster service with
                   declared ports

Use --no-build / --no-deploy to skip phases when iterating. Use
--cluster-only / --host-only to scope the orchestrator to one side of
the split (cluster CI / host-only debugging respectively).

With --background, the orchestrator detaches every long-running child
under ~/.cache/forge/up/<env>/ and returns immediately. Use
` + "`forge up stop --env=<env>`" + ` to terminate the tracked PIDs.

Examples:
  forge up --env=dev
  forge up --env=dev --no-build
  forge up --env=dev --cluster-only
  forge up --env=dev --background
  forge up stop --env=dev`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.env == "" {
				return fmt.Errorf("--env is required (e.g. --env=dev)")
			}
			if opts.clusterOnly && opts.hostOnly {
				return fmt.Errorf("--cluster-only and --host-only are mutually exclusive")
			}
			return runUp(cmd.Context(), opts)
		},
	}

	cmd.Flags().StringVar(&opts.env, "env", "", "Deploy environment to bring up (e.g. dev, staging) — required")
	cmd.Flags().BoolVar(&opts.noBuild, "no-build", false, "Skip the build phase (use already-built images / binaries)")
	cmd.Flags().BoolVar(&opts.noDeploy, "no-deploy", false, "Skip the deploy phase (don't kubectl apply)")
	cmd.Flags().BoolVar(&opts.clusterOnly, "cluster-only", false, "Only run cluster phases (build + deploy); skip host/frontend/port-forward")
	cmd.Flags().BoolVar(&opts.hostOnly, "host-only", false, "Only run host phases (host + frontend + port-forward); skip build/deploy")
	cmd.Flags().BoolVar(&opts.background, "background", false, "Detach long-running phases and return immediately (stop with `forge up stop --env=<env>`)")

	cmd.AddCommand(newUpStopCmd())
	return cmd
}

// newUpStopCmd reads the PID files written by `forge up --background`
// and terminates every tracked process. Idempotent: no-op when nothing
// is tracked.
func newUpStopCmd() *cobra.Command {
	var env string
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop background `forge up` processes for an environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			if env == "" {
				return fmt.Errorf("--env is required")
			}
			return runUpStop(env)
		},
	}
	cmd.Flags().StringVar(&env, "env", "", "Environment whose background processes to stop")
	return cmd
}

// runUp is the orchestrator. Returns the first error encountered in
// phases 1-2 (no point bringing host processes up against a busted
// cluster). Phases 3-5 are collected into the running-process set and
// torn down by the Ctrl-C cleanup cascade on exit.
func runUp(ctx context.Context, opts upOptions) error {
	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}
	projectDir := projectDirForKCL()

	fmt.Printf("[up] env=%s\n", opts.env)
	entities, err := RenderKCL(ctx, projectDir, opts.env)
	if err != nil {
		return fmt.Errorf("render KCL: %w", err)
	}
	if entitiesEmpty(entities) {
		return fmt.Errorf("no services/operators/frontends/cronjobs declared in deploy/kcl/%s/", opts.env)
	}
	summarizeKCLBuildPlan(entities)

	// Cluster phases — build + deploy.
	if !opts.hostOnly {
		if !opts.noBuild {
			fmt.Println("\n[up] build phase")
			if err := upBuildCluster(ctx, cfg, opts.env); err != nil {
				return fmt.Errorf("build: %w", err)
			}
		}
		if !opts.noDeploy {
			fmt.Println("\n[up] deploy phase")
			if err := upDeployCluster(ctx, opts.env); err != nil {
				return fmt.Errorf("deploy: %w", err)
			}
		}
	}

	// --cluster-only stops here: skip the host/frontend/port-forward
	// phases. Useful for CI lanes that only care about the apply.
	if opts.clusterOnly {
		fmt.Println("\n[up] --cluster-only: skipping host/frontend/port-forward phases")
		return nil
	}

	// Host phases — host services + frontends + port-forwards. These
	// are tracked under the orchestrator's child-process registry so
	// Ctrl-C tears them all down together.
	procs := newProcRegistry(opts.env)
	defer procs.shutdownOnExit()

	// Phase 3: host-mode services.
	hostFailures := upHostServices(ctx, cfg, entities, opts.env, opts.background, procs)
	if hostFailures > 0 {
		fmt.Printf("[up] %d host service(s) failed to start (see above)\n", hostFailures)
	}

	// Phase 4: frontends.
	feFailures := upFrontends(ctx, entities, opts.env, opts.background, procs)
	if feFailures > 0 {
		fmt.Printf("[up] %d frontend(s) failed to start (see above)\n", feFailures)
	}

	// Phase 5: port-forwards for cluster services with declared ports.
	pfFailures := upPortForwards(ctx, cfg, entities, opts.env, opts.background, procs)
	if pfFailures > 0 {
		fmt.Printf("[up] %d port-forward(s) failed to start (see above)\n", pfFailures)
	}

	if opts.background {
		fmt.Printf("\n[up] detached %d process(es). Stop with `forge up stop --env=%s`.\n",
			procs.count(), opts.env)
		procs.persist()
		return nil
	}

	if procs.count() == 0 {
		fmt.Println("\n[up] no host/frontend/port-forward processes to wait on; deploy is up.")
		return nil
	}

	fmt.Printf("\n[up] %d process(es) running. Ctrl-C to stop.\n", procs.count())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	fmt.Println("\n[up] shutting down...")
	procs.shutdown()
	return nil
}

// entitiesEmpty reports whether the entity set has zero declarations
// of every kind.
func entitiesEmpty(e *KCLEntities) bool {
	return e == nil || (len(e.Services) == 0 && len(e.Operators) == 0 && len(e.Frontends) == 0 && len(e.CronJobs) == 0)
}

// upBuildCluster builds + pushes the project docker image with the
// per-env KCL filter applied (deliverable 3's runBuild path). The
// registry comes from environments[].registry — defaults to
// localhost:5050 for dev (the canonical k3d mirror).
func upBuildCluster(ctx context.Context, cfg *config.ProjectConfig, env string) error {
	registry := "localhost:5050"
	if e := findEnvironment(cfg, env); e != nil && e.Registry != "" {
		registry = e.Registry
	}
	opts := buildOptions{
		outputDir:    "bin",
		buildTarget:  "all",
		parallel:     true,
		buildDocker:  true,
		pushRegistry: registry,
		env:          env,
	}
	return runBuild(ctx, opts)
}

// upDeployCluster invokes runDeploy with the per-env defaults. The
// deploy command itself reads KCL again to drive the rollout-wait
// skip and one-shot Job dispatch (deliverable 4).
func upDeployCluster(ctx context.Context, env string) error {
	return runDeploy(ctx, env, deployOptions{})
}

// upHostServices starts every host-mode service as a child process,
// dispatching on deploy.Host.Runner. Returns the count of services that
// failed to start (logged but not fatal — the orchestrator brings up as
// many as it can rather than bailing on the first failure).
func upHostServices(ctx context.Context, cfg *config.ProjectConfig, e *KCLEntities, env string, background bool, procs *procRegistry) int {
	failures := 0
	for _, svc := range e.Services {
		if svc.Deploy.Type != "host" || svc.Deploy.Host == nil {
			continue
		}
		cmd, name, err := buildHostServiceCmd(ctx, cfg, svc, env)
		if err != nil {
			fmt.Printf("[up] host %s: %v\n", svc.Name, err)
			failures++
			continue
		}
		if err := procs.start(name, cmd, background); err != nil {
			fmt.Printf("[up] host %s: %v\n", svc.Name, err)
			failures++
		}
	}
	return failures
}

// buildHostServiceCmd composes the exec.Cmd for a host-mode service
// based on its deploy.Host.Runner. Thin shim over hostlaunch.BuildCmd
// with the secrets_file + env_vars + forge.yaml config composition done
// here. cfg / env feed the projectConfig layer (forge.yaml
// environments[<env>].config); a nil cfg or empty env skips that layer
// without erroring.
//
// Env layering matches `forge run <svc>` exactly: projectConfig →
// secrets_file → env_vars → os.Environ() wins last. See
// hostlaunch.LayerHostEnv for the full precedence rationale.
//
// Unlike `forge run <svc>`, `forge up` is strict about unknown runners:
// a typo in KCL is fail-loud here because the orchestrator owns the
// whole environment and silent fallback to go-run could mask a deploy
// pin the user meant to apply. The hostlaunch package itself falls
// through to go-run on unknown runners; the explicit IsKnownRunner
// gate is what makes this call site strict.
func buildHostServiceCmd(ctx context.Context, cfg *config.ProjectConfig, svc ServiceEntity, env string) (*exec.Cmd, string, error) {
	host := svc.Deploy.Host
	if !hostlaunch.IsKnownRunner(host.Runner) {
		return nil, "", fmt.Errorf("unknown host runner %q (expected go-run/air/binary/delve)", host.Runner)
	}
	spec := hostlaunch.RunnerSpec{
		Runner:    host.Runner,
		AirConfig: host.AirConfig,
		DelvePort: host.DelvePort,
	}
	cmd := hostlaunch.BuildCmd(ctx, svc.Name, spec)

	// Env composition: projectConfig → secrets_file → env_vars →
	// os.Environ() wins last. Missing secrets_file is non-fatal
	// (warn-and-continue); parse / permission errors are fatal because
	// they signal a broken KCL pin rather than a developer who hasn't
	// created the file yet.
	secrets, lerr := hostlaunch.LoadSecretsFile(host.SecretsFile)
	switch {
	case lerr == nil:
	case errors.Is(lerr, os.ErrNotExist):
		fmt.Printf("[up] host %s: warning: secrets file %s missing; continuing without it\n", svc.Name, host.SecretsFile)
	default:
		return nil, "", fmt.Errorf("host %s: read secrets file %s: %w", svc.Name, host.SecretsFile, lerr)
	}
	envVars := hostEnvVarsToMap(host)
	// projectConfig layer: forge.yaml environments[<env>].config projected
	// to env-var strings. Same source the cluster ConfigMap projection
	// uses; layering it here keeps `forge up` host services in sync with
	// `forge run <svc>`. nil cfg / empty env collapses to an empty map.
	var projectConfigEnv map[string]string
	if cfg != nil && env != "" {
		projectConfigEnv = loadProjectConfigEnv(cfg, env)
	}
	cmd.Env = hostlaunch.LayerHostEnv(os.Environ(), projectConfigEnv, secrets, envVars)

	return cmd, svc.Name, nil
}

// upFrontends starts every declared frontend in its path. DevRunner
// defaults to npm; we don't try yarn/pnpm fallback magic — if the
// project uses pnpm, declare dev_runner: pnpm in KCL.
func upFrontends(ctx context.Context, e *KCLEntities, env string, background bool, procs *procRegistry) int {
	failures := 0
	for _, fe := range e.Frontends {
		runner := fe.DevRunner
		if runner == "" {
			runner = "npm"
		}
		cmd := exec.CommandContext(ctx, runner, "run", "dev")
		cmd.Dir = fe.Path

		envFile := fe.EnvFile
		if envFile == "" {
			envFile = ".env." + env
		}
		extra, lerr := hostlaunch.ReadDotEnvFile(envFile)
		if lerr == nil {
			cmd.Env = hostlaunch.MergeEnv(extra, os.Environ())
		}

		if err := procs.start("frontend:"+fe.Name, cmd, background); err != nil {
			fmt.Printf("[up] frontend %s: %v\n", fe.Name, err)
			failures++
		}
	}
	return failures
}

// upPortForwards starts a `kubectl port-forward` for every cluster
// service with declared ports. Local port == remote port (the simplest
// shape; users with conflicts can fall back to `forge dev port-forward
// --local <p>:<r>` manually). Service-name → deployment-name uses the
// shared-binary prefix when forge.yaml declares binary=shared.
func upPortForwards(ctx context.Context, cfg *config.ProjectConfig, e *KCLEntities, env string, background bool, procs *procRegistry) int {
	namespace := cfg.Name + "-" + env
	if envCfg := findEnvironment(cfg, env); envCfg != nil && envCfg.Namespace != "" {
		namespace = envCfg.Namespace
	}

	failures := 0
	for _, svc := range e.Services {
		if svc.Deploy.Type != "cluster" || svc.Deploy.Cluster == nil {
			continue
		}
		for _, port := range svc.Deploy.Cluster.Ports {
			depName := svc.Name
			if cfg.IsBinaryShared() {
				depName = cfg.Name + "-" + svc.Name
			}
			pfName := fmt.Sprintf("pf:%s:%d", svc.Name, port)
			pfArgs := []string{"port-forward",
				"-n", namespace,
				"deployment/" + depName,
				fmt.Sprintf("%d:%d", port, port),
			}
			cmd := exec.CommandContext(ctx, "kubectl", pfArgs...)
			if err := procs.start(pfName, cmd, background); err != nil {
				fmt.Printf("[up] %s: %v\n", pfName, err)
				failures++
			}
		}
	}
	return failures
}

// procRegistry tracks long-running child processes started by the up
// orchestrator and provides the cleanup cascade for Ctrl-C teardown.
// Background mode persists PIDs under ~/.cache/forge/up/<env>/ so
// `forge up stop --env=<env>` can find them.
type procRegistry struct {
	env       string
	mu        sync.Mutex
	processes []*managedProcess
	persisted bool
}

func newProcRegistry(env string) *procRegistry {
	return &procRegistry{env: env}
}

// start launches a child command, captures stdout/stderr with a
// `[<name>]` prefix, and registers the process for later teardown.
// When background==true, stdout/stderr are sent to per-service log
// files under ~/.cache/forge/up/<env>/<name>.log so the user can
// `tail -f` them after detach.
func (p *procRegistry) start(name string, cmd *exec.Cmd, background bool) error {
	if background {
		logPath, err := upLogPath(p.env, name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			return err
		}
		logFile, err := os.Create(logPath)
		if err != nil {
			return err
		}
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err := cmd.Start(); err != nil {
			_ = logFile.Close()
			return err
		}
		if cmd.Process != nil {
			_ = cmd.Process.Release()
		}
		p.mu.Lock()
		p.processes = append(p.processes, &managedProcess{name: name, cmd: cmd})
		p.mu.Unlock()
		fmt.Printf("[up] %s: detached (pid=%d, log=%s)\n", name, cmd.Process.Pid, logPath)
		return nil
	}

	prefix := fmt.Sprintf("[%s] ", name)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe %s: %w", name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe %s: %w", name, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	go streamUpOutput(prefix, stdout)
	go streamUpOutput(prefix, stderr)

	p.mu.Lock()
	p.processes = append(p.processes, &managedProcess{name: name, cmd: cmd})
	p.mu.Unlock()
	fmt.Printf("[up] %s: started (pid=%d)\n", name, cmd.Process.Pid)
	return nil
}

// streamUpOutput tags each child line with [<name>] and writes it to
// the orchestrator's stdout. Kept separate from the run.go variant so
// the up orchestrator owns its log convention.
func streamUpOutput(prefix string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), 1024*1024)
	for scanner.Scan() {
		fmt.Print(prefix + scanner.Text() + "\n")
	}
}

// count returns the registered process count.
func (p *procRegistry) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.processes)
}

// persist writes the tracked PID set to disk so `forge up stop` can
// teardown after detach. Each line is `<name>\t<pid>`.
func (p *procRegistry) persist() {
	if p.persisted {
		return
	}
	p.persisted = true
	statePath, err := upStatePath(p.env)
	if err != nil {
		fmt.Printf("[up] warning: resolve state path: %v\n", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		fmt.Printf("[up] warning: mkdir state: %v\n", err)
		return
	}
	var b strings.Builder
	p.mu.Lock()
	for _, mp := range p.processes {
		if mp.cmd.Process == nil {
			continue
		}
		fmt.Fprintf(&b, "%s\t%d\n", mp.name, mp.cmd.Process.Pid)
	}
	p.mu.Unlock()
	if err := os.WriteFile(statePath, []byte(b.String()), 0o644); err != nil {
		fmt.Printf("[up] warning: write state: %v\n", err)
	}
}

// shutdown sends SIGTERM to every registered process and waits up to
// 10s for them to exit. Anything still alive after the budget is
// SIGKILLed. State file is removed on success.
func (p *procRegistry) shutdown() {
	p.mu.Lock()
	procs := make([]*managedProcess, len(p.processes))
	copy(procs, p.processes)
	p.mu.Unlock()

	// Reverse order so port-forwards die before the services they
	// forward to — keeps the user's last log lines clean.
	for i := len(procs) - 1; i >= 0; i-- {
		mp := procs[i]
		if mp.cmd.Process == nil {
			continue
		}
		fmt.Printf("[up] stopping %s (pid=%d)...\n", mp.name, mp.cmd.Process.Pid)
		_ = mp.cmd.Process.Signal(syscall.SIGTERM)
	}

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, mp := range procs {
			mp := mp
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = mp.cmd.Wait()
			}()
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		for _, mp := range procs {
			if mp.cmd.Process == nil {
				continue
			}
			fmt.Printf("[up] %s: did not exit, killing.\n", mp.name)
			_ = mp.cmd.Process.Kill()
		}
		<-done
	}

	statePath, err := upStatePath(p.env)
	if err == nil {
		_ = os.Remove(statePath)
	}
}

// shutdownOnExit is the defer-friendly shutdown wrapper that only fires
// when the orchestrator is exiting through a panic / unexpected path.
// Background mode (procs.persisted == true) skips it — the state file
// is the contract with `forge up stop`.
func (p *procRegistry) shutdownOnExit() {
	if p.persisted {
		return
	}
	p.mu.Lock()
	hasProcs := len(p.processes) > 0
	p.mu.Unlock()
	if !hasProcs {
		return
	}
	// Already torn down by the Ctrl-C handler in runUp — no-op.
	// Only fires if runUp returned without reaching <-sigCh, which
	// happens when count()==0 or in test contexts.
}

// runUpStop reads the persisted state file and SIGTERMs every tracked
// pid. Missing state file is a friendly no-op so the command is safe
// to invoke unconditionally on teardown.
func runUpStop(env string) error {
	statePath, err := upStatePath(env)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("[up] no tracked processes for env=%s (state file missing).\n", env)
			return nil
		}
		return fmt.Errorf("read state: %w", err)
	}
	var stopped int
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(parts[1], "%d", &pid); err != nil {
			continue
		}
		proc, ferr := os.FindProcess(pid)
		if ferr != nil {
			fmt.Printf("[up] %s: find pid %d: %v\n", parts[0], pid, ferr)
			continue
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			fmt.Printf("[up] %s: signal pid %d: %v (already exited?)\n", parts[0], pid, err)
		} else {
			fmt.Printf("[up] %s: SIGTERM sent to pid %d\n", parts[0], pid)
			stopped++
		}
	}
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("[up] warning: remove state: %v\n", err)
	}
	fmt.Printf("[up] stopped %d process(es).\n", stopped)
	return nil
}

// upStatePath returns the per-env state file location:
//
//	$HOME/.cache/forge/up/<env>.pids
//
// Used by --background to persist tracked PIDs and by `forge up stop`
// to read them back.
func upStatePath(env string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".cache", "forge", "up", env+".pids"), nil
}

// upLogPath returns the per-(env,process) log file location for
// detached children:
//
//	$HOME/.cache/forge/up/<env>/<name>.log
//
// The `name` is sanitised (slashes → underscores) so it's safe to use
// as a path component.
func upLogPath(env, name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	safe := strings.ReplaceAll(strings.ReplaceAll(name, "/", "_"), ":", "_")
	return filepath.Join(home, ".cache", "forge", "up", env, safe+".log"), nil
}
