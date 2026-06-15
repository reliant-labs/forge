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
//  6. Wait Ctrl-C → cascade cleanup → exit.
//
// Reaching cluster services from the host is the Gateway API ingress
// path (see `forge cluster urls`); ad-hoc shells against stateful workloads
// stay available via `kubectl port-forward` directly.
//
// Replaces the dev-loop bash script every forge project would otherwise
// hand-write to coordinate build + deploy + run.
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
	"github.com/reliant-labs/forge/internal/kclplugin"
)

// upOptions bundles flags for `forge up`.
type upOptions struct {
	env         string
	noBuild     bool
	noDeploy    bool
	clusterOnly bool // build + deploy cluster manifests, skip host/frontend
	hostOnly    bool // skip cluster build+deploy, run host + frontend phases only
	background  bool // detach and write PID files; use `forge up stop --env=<env>` to teardown
	noGenerate  bool // skip the pre-build "ensure generated code" step (--no-generate)
	noInstall   bool // skip the pre-dev-serve "ensure frontend deps" step (--no-install)
}

func newUpCmd() *cobra.Command {
	var opts upOptions

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Bring the whole dev loop up: build + deploy + host + frontend",
		Long: `Bring the whole dev loop up for an environment.

Reads deploy/kcl/<env>/ to figure out which services run in-cluster vs
on the host and which frontends to start.

Phases:
  1. build:    docker build + push every cluster image; go build
               each build-only variant
  2. deploy:   kubectl apply cluster manifests; wait rollouts and
               one-shot Jobs
  3. host:     start every host-mode service (go-run / air / binary
               / delve)
  4. frontend: start every declared frontend in its path

Reaching cluster services from the host is the Gateway API ingress
path; run ` + "`forge cluster urls`" + ` to list the routes.

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
	cmd.Flags().BoolVar(&opts.noDeploy, "no-deploy", false, "Skip the cluster apply phase (host services and frontends still launch)")
	cmd.Flags().BoolVar(&opts.clusterOnly, "cluster-only", false, "Only run cluster phases (build + deploy); skip host/frontend")
	cmd.Flags().BoolVar(&opts.hostOnly, "host-only", false, "Only run host phases (host + frontend); skip build/deploy")
	cmd.Flags().BoolVar(&opts.background, "background", false, "Detach long-running phases and return immediately (stop with `forge up stop --env=<env>`)")
	cmd.Flags().BoolVar(&opts.noGenerate, "no-generate", false, "Skip the pre-build code-generation check. By default `forge up` runs `forge generate` when gen/ is missing or proto sources are newer than the generated tree.")
	cmd.Flags().BoolVar(&opts.noInstall, "no-install", false, "Skip the pre-dev-serve frontend dependency install. By default `forge up` installs a frontend's deps when node_modules is missing or older than its lockfile/manifest.")

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
// cluster). Phases 3-4 are collected into the running-process set and
// torn down by the Ctrl-C cleanup cascade on exit.
func runUp(ctx context.Context, opts upOptions) error {
	store, err := loadProjectStore()
	if err != nil {
		return err
	}
	cfg := store.Config()
	projectDir := projectDirForKCL()

	// Persist resolve_port allocations per env so dev ports stay stable
	// across `forge up` runs (reliant-web keeps its port, etc.). Only the
	// dev-launch path opts in — read-only renders (forge ci) don't write
	// a ports file. The store is machine-local; .forge/ports-*.json is
	// gitignored.
	kclplugin.UsePortStore(filepath.Join(projectDir, ".forge", "ports-"+opts.env+".json"))

	fmt.Printf("[up] env=%s\n", opts.env)
	entities, err := RenderKCL(ctx, projectDir, opts.env)
	if err != nil {
		return fmt.Errorf("render KCL: %w", err)
	}
	if entitiesEmpty(entities) {
		return fmt.Errorf("no services/operators/frontends/cronjobs declared in deploy/kcl/%s/", opts.env)
	}
	summarizeKCLBuildPlan(entities)

	// Cluster phases — build + deploy. Both are feature-gated: if the
	// project's forge.yaml turns either off (`features.build: false`
	// or `features.deploy: false`), the orchestrator skips the phase
	// with a one-line log and continues. Direct `forge build` /
	// `forge deploy` invocations still error — see requireFeature
	// in feature_gate.go for the strict-gate shape used by the cobra
	// RunE for those commands.
	if !opts.hostOnly {
		if !opts.noBuild {
			if !skipFeature(store, config.FeatureBuild, "up:build") {
				fmt.Println("\n[up] build phase")
				if err := upBuildCluster(ctx, cfg, opts.env, opts.noGenerate); err != nil {
					return fmt.Errorf("build: %w", err)
				}
			}
		}
		if !opts.noDeploy {
			if !skipFeature(store, config.FeatureDeploy, "up:deploy") {
				fmt.Println("\n[up] deploy phase")
				if err := upDeployCluster(ctx, opts.env); err != nil {
					return fmt.Errorf("deploy: %w", err)
				}
			}
		}
	}

	// --cluster-only stops here: skip the host/frontend phases. Useful
	// for CI lanes that only care about the apply.
	if opts.clusterOnly {
		fmt.Println("\n[up] --cluster-only: skipping host/frontend phases")
		return nil
	}

	// When the build phase was skipped (--host-only / --no-build) the
	// host runners (air / go-run) still compile against gen/, so ensure
	// generated code is present here too — otherwise host services fail
	// with the same "cannot load module gen" error the build phase would
	// have pre-empted. No-op when up-to-date or --no-generate. (The
	// non-skipped path already ran this inside runBuild.)
	if opts.hostOnly || opts.noBuild {
		if err := ensureGeneratedCode(projectDirForKCL(), opts.noGenerate); err != nil {
			return fmt.Errorf("ensure generated code: %w", err)
		}
	}

	// Host phases — host services + frontends. These are tracked under
	// the orchestrator's child-process registry so Ctrl-C tears them
	// all down together.
	procs := newProcRegistry(opts.env)
	defer procs.shutdownOnExit()

	// Phase 3: host-mode services.
	hostFailures := upHostServices(ctx, cfg, entities, opts.env, opts.background, procs)
	if hostFailures > 0 {
		fmt.Printf("[up] %d host service(s) failed to start (see above)\n", hostFailures)
	}

	// Phase 4: frontends. Skipped (with a log line) when
	// features.frontend: false — the orchestrator otherwise tries to
	// npm-run-dev a tree that the project never scaffolded.
	if !skipFeature(store, config.FeatureFrontend, "up:frontend") {
		feFailures := upFrontends(ctx, entities, opts.env, opts.background, opts.noInstall, procs)
		if feFailures > 0 {
			fmt.Printf("[up] %d frontend(s) failed to start (see above)\n", feFailures)
		}
	}

	// Summary box: what's listening where, and where to find each
	// service's log. Printed in both foreground and background so the
	// URLs + log paths are one glance away (and greppable for an agent).
	printUpSummary(entities, opts.env, opts.background)

	if opts.background {
		fmt.Printf("[up] detached %d process(es). Stop with `forge up stop --env=%s`.\n",
			procs.count(), opts.env)
		procs.persist()
		return nil
	}

	if procs.count() == 0 {
		fmt.Println("[up] no host/frontend processes to wait on; deploy is up.")
		return nil
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	fmt.Println("\n[up] shutting down...")
	procs.shutdown()
	return nil
}

// printUpSummary prints a compact box of what `forge up` just brought
// up: each host service and frontend, its URL (when a listen port is
// known), and the path to its log file — plus where to grep all logs
// and how to list cluster routes. Mirrors the cloud-dev "final banner"
// so a developer (or an LLM agent) can find URLs and logs at a glance
// instead of scraping them out of interleaved startup scrollback.
//
// Best-effort: host-service ports are read from the KCL `PORT` env var
// (the bind-port convention); a service that doesn't declare one is
// listed without a URL. No-op when nothing host/frontend ran.
func printUpSummary(e *KCLEntities, env string, background bool) {
	if e == nil {
		return
	}
	type row struct{ name, url, log string }
	var hosts, fronts []row
	for _, svc := range e.Services {
		if svc.Deploy.Type != "host" || svc.Deploy.Host == nil {
			continue
		}
		url := ""
		if p := hostEnvPort(svc.Name, svc.Deploy.Host); p != "" {
			url = "http://localhost:" + p
		}
		hosts = append(hosts, row{svc.Name, url, summaryLogPath(env, svc.Name)})
	}
	for _, fe := range e.Frontends {
		url := ""
		if fe.Port > 0 {
			url = fmt.Sprintf("http://localhost:%d", fe.Port)
		}
		fronts = append(fronts, row{fe.Name, url, summaryLogPath(env, "frontend:"+fe.Name)})
	}
	if len(hosts) == 0 && len(fronts) == 0 {
		return
	}

	const bar = "│"
	line := func(name, url, log string) {
		if url != "" {
			fmt.Printf("%s   %-22s %s\n", bar, name, url)
		} else {
			fmt.Printf("%s   %s\n", bar, name)
		}
		fmt.Printf("%s     ↳ %s\n", bar, log)
	}

	fmt.Println()
	fmt.Printf("╭─ forge up · env=%s ─────────────────────────────────────\n", env)
	if len(hosts) > 0 {
		fmt.Printf("%s Host services\n", bar)
		for _, r := range hosts {
			line(r.name, r.url, r.log)
		}
	}
	if len(fronts) > 0 {
		fmt.Printf("%s Frontends\n", bar)
		for _, r := range fronts {
			line(r.name, r.url, r.log)
		}
	}
	fmt.Printf("%s\n", bar)
	fmt.Printf("%s Logs   %s/   — tail -f / grep the per-service *.log here\n", bar, upLogDir(env))
	fmt.Printf("%s Cluster routes:  forge cluster urls\n", bar)
	if background {
		fmt.Printf("%s Detached — stop with `forge up stop --env=%s`\n", bar, env)
	} else {
		fmt.Printf("%s Ctrl-C to stop.\n", bar)
	}
	fmt.Println("╰─────────────────────────────────────────────────────────")
	fmt.Println()
}

// summaryLogPath returns the display (project-relative) log path for a
// started process, matching the file upLogPath actually writes. Kept in
// sync with upLogPath's name sanitisation so the printed path is the one
// a `grep`/`tail` will find.
func summaryLogPath(env, name string) string {
	safe := strings.ReplaceAll(strings.ReplaceAll(name, "/", "_"), ":", "_")
	return filepath.Join(upLogDir(env), safe+".log")
}

// hostEnvPort returns the host service's declared listen port from its
// env vars, or "" when none is declared. It prefers a service-specific
// <NAME>_PORT (e.g. ADMIN_SERVER_PORT for "admin-server") over the
// generic PORT: a service that declares both usually binds the specific
// one, and the generic PORT is often a vestigial default the binary
// ignores (cp-forge's admin-server sets PORT=8080 but actually binds
// ADMIN_SERVER_PORT=8090). Only the inline `value` channel applies —
// config_map_ref / secret_ref ports have no host-side literal to show.
func hostEnvPort(name string, host *HostDeploy) string {
	if host == nil {
		return ""
	}
	specific := strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_PORT"
	generic := ""
	for _, ev := range host.EnvVars {
		if ev.Value == "" {
			continue
		}
		switch ev.Name {
		case specific:
			return ev.Value
		case "PORT":
			generic = ev.Value
		}
	}
	return generic
}

// entitiesEmpty reports whether the entity set has zero declarations
// of every kind.
func entitiesEmpty(e *KCLEntities) bool {
	return e == nil || (len(e.Services) == 0 && len(e.Operators) == 0 && len(e.Frontends) == 0 && len(e.CronJobs) == 0)
}

// upBuildCluster builds + pushes the project docker image with the
// per-env KCL filter applied (deliverable 3's runBuild path). The
// registry comes from the rendered KCL's K8sCluster.registry —
// defaults to localhost:5050 for dev (the canonical k3d mirror).
func upBuildCluster(ctx context.Context, _ *config.ProjectConfig, env string, noGenerate bool) error {
	registry := "localhost:5050"
	if reg := k8sClusterRegistryForEnv(ctx, env); reg != "" {
		registry = reg
	}
	opts := buildOptions{
		outputDir:     "bin",
		buildTarget:   "all",
		parallel:      true,
		buildDocker:   true,
		pushRegistry:  registry,
		env:           env,
		skipFrontends: true,
		skipGenerate:  noGenerate,
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
	// An explicit `command` overrides the runner convention (sibling-repo
	// binaries, non-standard entrypoints), so the strict runner-name check
	// only applies when no command is given.
	if len(svc.Command) == 0 && !hostlaunch.IsKnownRunner(host.Runner) {
		return nil, "", fmt.Errorf("unknown host runner %q (expected go-run/air/binary/delve)", host.Runner)
	}
	spec := hostlaunch.RunnerSpec{
		Runner:     host.Runner,
		AirConfig:  host.AirConfig,
		DelvePort:  host.DelvePort,
		WorkingDir: host.WorkingDir,
		ProjectDir: projectDirForKCL(),
		Command:    svc.Command,
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
//
// Each frontend's dependencies are ensured first (unless noInstall): a
// missing or stale node_modules is installed before `npm run dev`, so a
// fresh checkout doesn't fail with "next: command not found". A failed
// install counts as a frontend failure (logged, non-fatal) so the rest
// of the loop still comes up.
func upFrontends(ctx context.Context, e *KCLEntities, env string, background, noInstall bool, procs *procRegistry) int {
	failures := 0
	for _, fe := range e.Frontends {
		if err := ensureFrontendDeps(ctx, fe, noInstall); err != nil {
			fmt.Printf("[up] frontend %s: %v\n", fe.Name, err)
			failures++
			continue
		}
		cmd := buildFrontendCmd(ctx, fe, env, os.Environ())
		if err := procs.start("frontend:"+fe.Name, cmd, background); err != nil {
			fmt.Printf("[up] frontend %s: %v\n", fe.Name, err)
			failures++
		}
	}
	return failures
}

// ensureFrontendDeps installs a frontend's node_modules when they are
// missing or stale relative to its lockfile/manifest, so `npm run dev`
// doesn't fail with "next: command not found" on a fresh checkout. No-op
// when noInstall is set, when the path has no package.json (not a node
// project), or when deps are already up to date. Mirrors
// ensureGeneratedCode's staleness gate for the frontend half of the loop.
//
// The install verb follows the frontend's dev_runner (npm/pnpm/yarn).
// `npm install` (not `npm ci`) is used deliberately: it reconciles a
// missing or partial tree and tolerates a slightly-drifted lockfile
// rather than hard-failing the dev loop the way `npm ci` would.
func ensureFrontendDeps(ctx context.Context, fe FrontendEntity, noInstall bool) error {
	if noInstall || fe.Path == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(fe.Path, "package.json")); err != nil {
		return nil // not a node project (or no manifest) — nothing to install
	}
	if !frontendDepsStale(fe.Path) {
		return nil
	}
	runner := fe.DevRunner
	if runner == "" {
		runner = "npm"
	}
	fmt.Printf("[up] %s: node_modules missing/stale — running `%s install`\n", fe.Name, runner)
	cmd := exec.CommandContext(ctx, runner, "install")
	cmd.Dir = fe.Path
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install deps in %s: %w", fe.Path, err)
	}
	return nil
}

// frontendDepsStale reports whether a frontend's node_modules is missing
// or older than its lockfile/manifest — the cheap staleness gate that
// keeps ensureFrontendDeps a no-op in the steady state. node_modules'
// directory mtime is bumped by every install, so a lockfile/manifest
// edit (or a never-installed tree) is what trips this.
func frontendDepsStale(dir string) bool {
	nm, err := os.Stat(filepath.Join(dir, "node_modules"))
	if err != nil {
		return true // missing → must install
	}
	nmTime := nm.ModTime()
	for _, manifest := range []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock", "package.json"} {
		if info, err := os.Stat(filepath.Join(dir, manifest)); err == nil {
			if info.ModTime().After(nmTime) {
				return true
			}
		}
	}
	return false
}

// buildFrontendCmd composes the *exec.Cmd for a single frontend in the
// up orchestrator. Split out from upFrontends so the env composition is
// testable without launching a child process.
//
// Env layering:
//
//  1. The env-file (fe.EnvFile or `.env.<env>`) is the lowest layer.
//  2. KCL-declared env_vars layer on top of the env-file — explicit
//     per-env config (e.g. a VITE_ADMIN_URL composed from
//     forge.resolve_port(...)) beats the generic env-file.
//  3. parentEnv (os.Environ()) wins over both — developer-shell override
//     semantics, matching the host-service shape.
//  4. PORT from the KCL declaration is force-injected last so it
//     overrides ANY PORT in the parent env. The KCL declaration is the
//     canonical port binding for the dev loop; a stale `PORT=8080` in
//     the parent shell (typical when the parent has another service's
//     env exported) can't silently shift the bind port out from under
//     the user. Same precedence as KCL EnvVars on host services.
//
// fe.Port == 0 (legacy projects that don't set the field) skips the
// force-inject so we don't surface a meaningless "PORT=0" line that
// would crash the dev server.
func buildFrontendCmd(ctx context.Context, fe FrontendEntity, env string, parentEnv []string) *exec.Cmd {
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
	// Precedence (low→high): env-file < KCL env_vars < parent shell, then
	// forced PORT (below). Mirrors host-service layering (LayerHostEnv):
	// explicit per-env KCL config beats the generic env-file, the
	// developer's shell can still override, and the KCL port binding wins
	// last. Missing env-file is non-fatal (nil map collapses to no-op).
	envFileMap, _ := hostlaunch.ReadDotEnvFile(envFile)
	cmd.Env = hostlaunch.LayerHostEnv(parentEnv, envFileMap, nil, kclEnvVarsToMap(fe.EnvVars))

	if fe.Port > 0 {
		cmd.Env = withForcedEnv(cmd.Env, "PORT", fmt.Sprintf("%d", fe.Port))
	}
	return cmd
}

// kclEnvVarsToMap projects a frontend's KCL-declared env_vars to a
// name→value map, dropping entries without an inline value (secret_ref /
// config_map_ref entries are cluster-manifest concerns, not host-launch
// env). Mirrors hostEnvVarsToMap for host services.
func kclEnvVarsToMap(vars []KCLEnvVar) map[string]string {
	out := make(map[string]string, len(vars))
	for _, ev := range vars {
		if ev.Name == "" || ev.Value == "" {
			continue
		}
		out[ev.Name] = ev.Value
	}
	return out
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
		startInOwnProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			_ = logFile.Close()
			return err
		}
		// Capture the PID BEFORE Release() — Release resets
		// cmd.Process.Pid to -1, and persist()/the log line below need the
		// real PID so `forge up stop` can later SIGTERM it.
		pid := 0
		if cmd.Process != nil {
			pid = cmd.Process.Pid
			_ = cmd.Process.Release()
		}
		p.mu.Lock()
		p.processes = append(p.processes, &managedProcess{name: name, cmd: cmd, pid: pid})
		p.mu.Unlock()
		fmt.Printf("[up] %s: detached (pid=%d, log=%s)\n", name, pid, logPath)
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
	// Tee a raw copy of the child's output to its well-known log file so
	// it stays greppable after the fact, even in foreground mode where
	// the live stream is the interleaved, prefixed terminal output. A
	// failure to open the log file is non-fatal — the live stream still
	// works; we just warn and carry on without the file sink. The single
	// *os.File is shared by the stdout+stderr goroutines through a
	// lockedWriter so their line writes don't interleave mid-line.
	var sink io.Writer
	if logPath, perr := upLogPath(p.env, name); perr == nil {
		if mkErr := os.MkdirAll(filepath.Dir(logPath), 0o755); mkErr == nil {
			if f, ferr := os.Create(logPath); ferr == nil {
				sink = &lockedWriter{w: f}
			} else {
				fmt.Printf("[up] %s: warning: cannot open log file %s: %v\n", name, logPath, ferr)
			}
		}
	}

	startInOwnProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	go streamUpOutput(prefix, stdout, sink)
	go streamUpOutput(prefix, stderr, sink)

	p.mu.Lock()
	p.processes = append(p.processes, &managedProcess{name: name, cmd: cmd, pid: cmd.Process.Pid})
	p.mu.Unlock()
	fmt.Printf("[up] %s: started (pid=%d)\n", name, cmd.Process.Pid)
	return nil
}

// streamUpOutput tags each child line with [<name>] and writes it to the
// orchestrator's stdout. When logSink is non-nil it also writes the raw
// (un-prefixed) line there — the foreground file-tee. Kept separate from
// the run.go variant so the up orchestrator owns its log convention.
func streamUpOutput(prefix string, r io.Reader, logSink io.Writer) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Print(prefix + line + "\n")
		if logSink != nil {
			fmt.Fprintln(logSink, line)
		}
	}
}

// lockedWriter serialises writes from the concurrent stdout/stderr
// stream goroutines onto a single log file so their lines don't corrupt
// each other mid-write.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
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
		// Prefer the PID captured at Start (survives Process.Release on
		// the detach path); fall back to the live handle for any process
		// registered without a captured PID. Skip never-started / already-
		// released-without-capture entries so we never persist a 0/-1 that
		// `forge up stop` would try to signal.
		pid := mp.pid
		if pid == 0 && mp.cmd.Process != nil {
			pid = mp.cmd.Process.Pid
		}
		if pid <= 0 {
			continue
		}
		fmt.Fprintf(&b, "%s\t%d\n", mp.name, pid)
	}
	p.mu.Unlock()
	if err := os.WriteFile(statePath, []byte(b.String()), 0o644); err != nil {
		fmt.Printf("[up] warning: write state: %v\n", err)
	}
}

// procPID returns the best-known PID for a managed process: the value
// captured at Start (which survives Process.Release on the detach path),
// falling back to the live handle. Zero when never started — callers
// skip those so a 0 can't become a group-signal footgun.
func procPID(mp *managedProcess) int {
	if mp.pid > 0 {
		return mp.pid
	}
	if mp.cmd != nil && mp.cmd.Process != nil {
		return mp.cmd.Process.Pid
	}
	return 0
}

// shutdown sends SIGTERM to every registered process group and waits up
// to 10s for them to exit. Anything still alive after the budget is
// SIGKILLed. State file is removed on success.
func (p *procRegistry) shutdown() {
	p.mu.Lock()
	procs := make([]*managedProcess, len(p.processes))
	copy(procs, p.processes)
	p.mu.Unlock()

	// Reverse order so later-started frontends die before the host
	// services they may have spoken to — keeps the user's last log
	// lines clean.
	for i := len(procs) - 1; i >= 0; i-- {
		mp := procs[i]
		pid := procPID(mp)
		if pid <= 0 {
			continue
		}
		fmt.Printf("[up] stopping %s (pid=%d)...\n", mp.name, pid)
		// Signal the whole process group so `go run`'s execed child (and
		// any other grandchildren) die with the parent instead of
		// orphaning and squatting on their ports.
		_ = signalProcessGroup(pid, syscall.SIGTERM)
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
			pid := procPID(mp)
			if pid <= 0 {
				continue
			}
			fmt.Printf("[up] %s: did not exit, killing.\n", mp.name)
			_ = signalProcessGroup(pid, syscall.SIGKILL)
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
		// Guard against a non-positive PID slipping into the state file
		// (e.g. a pre-fix file written with a Release()'d -1). Signalling
		// pid -1 / 0 is a footgun — on Unix kill(-1) targets every process
		// the user can reach — so refuse rather than risk it.
		if pid <= 0 {
			fmt.Printf("[up] %s: skipping invalid pid %d in state file\n", parts[0], pid)
			continue
		}
		// Signal the whole process group (negative pid) so a detached
		// `go run`'s execed child dies too — the persisted pid is the
		// group leader (Setpgid at Start made pgid == pid).
		if err := signalProcessGroup(pid, syscall.SIGTERM); err != nil {
			fmt.Printf("[up] %s: signal pid %d: %v (already exited?)\n", parts[0], pid, err)
		} else {
			fmt.Printf("[up] %s: SIGTERM sent to group %d\n", parts[0], pid)
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
// upLogPath returns the well-known log file for a host service or
// frontend started by `forge up`. Logs land under the project-relative
// .forge/logs/<env>/ directory — gitignored via the `.forge/*` rule, and
// a stable, greppable location so a human (or an LLM agent working in the
// repo) can `tail -f` / `grep` one service's output without scraping it
// out of the interleaved terminal scrollback.
//
// Used by BOTH modes: background writes here as the sole sink; foreground
// tees a raw copy here alongside the live `[name]`-prefixed terminal
// stream. The `<name>` is sanitised ("/" and ":" → "_") so frontend
// labels like "frontend:web" map to a flat filename.
func upLogPath(env, name string) (string, error) {
	safe := strings.ReplaceAll(strings.ReplaceAll(name, "/", "_"), ":", "_")
	return filepath.Join(projectDirForKCL(), ".forge", "logs", env, safe+".log"), nil
}

// upLogDir returns the directory upLogPath writes into, for the summary's
// "grep here" pointer.
func upLogDir(env string) string {
	return filepath.Join(".forge", "logs", env)
}
