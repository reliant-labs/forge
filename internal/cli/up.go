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
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/envutil"
	"github.com/reliant-labs/forge/internal/hostlaunch"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/projectstore"
	"github.com/reliant-labs/forge/internal/secrets"
)

// upOptions bundles flags for `forge up`.
type upOptions struct {
	env         string
	noBuild     bool
	noDeploy    bool
	clusterOnly bool // build + deploy cluster manifests, skip host/frontend
	hostOnly    bool // skip cluster build+deploy, run host + frontend phases only
	background  bool // detach and write PID files; use `forge up stop --env=<env>` to teardown
	watch       bool // force supervise (hold + Ctrl-C teardown) even without a TTY
	restart     bool // stop any stack already running for this env first, then up
	noGenerate  bool // skip the pre-build "ensure generated code" step (--no-generate)
	noInstall   bool // skip the pre-dev-serve "ensure frontend deps" step (--no-install)
	// targets, when non-empty, scopes the host + frontend phases to the
	// named services/frontends — `forge up --target admin-server` is the
	// single-service inner loop (combine with --host-only to skip the
	// cluster build+deploy). Empty means "everything", the default.
	targets []string
}

// inTargetSet reports whether name should run given the --target filter.
// An empty filter matches everything (the default).
func inTargetSet(targets []string, name string) bool {
	if len(targets) == 0 {
		return true
	}
	for _, t := range targets {
		if t == name {
			return true
		}
	}
	return false
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

Lifecycle (what happens after host services + frontends start):

  * With a TTY (interactive shell): forge holds the foreground and
    tears the whole stack down on Ctrl-C.
  * Without a TTY (agent / CI / piped): forge brings everything up,
    prints the summary (URLs + per-service log paths), and RETURNS,
    leaving the processes running — the same end-state as --background.
    This is what keeps ` + "`forge up --env=<env>`" + ` from hanging an agent.
  * --watch forces the hold-and-teardown lifecycle even without a TTY
    (for a human piping the output through a tool).
  * --background always detaches and returns immediately, regardless of
    the TTY. If both --watch and --background are passed, --background
    wins (detach + return).

Either way the long-running children are tracked under
~/.cache/forge/up/<env>/; stop a detached / non-TTY stack with
` + "`forge up stop --env=<env>`" + `.

Examples:
  forge up --env=dev
  forge up --env=dev --no-build
  forge up --env=dev --cluster-only
  forge up --env=dev --watch        # hold + Ctrl-C teardown even when piped
  forge up --env=dev --background
  forge up stop --env=dev`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.env == "" {
				return fmt.Errorf("--env is required (e.g. --env=dev)")
			}
			if opts.clusterOnly && opts.hostOnly {
				return fmt.Errorf("--cluster-only and --host-only are mutually exclusive")
			}
			// --watch and --background both override the TTY default, in
			// opposite directions (hold vs detach). They are not combinable:
			// resolveUpLifecycle documents --background as the winner, but a
			// user passing both almost certainly has a mistaken mental model,
			// so reject it loudly rather than silently picking one.
			if opts.watch && opts.background {
				return fmt.Errorf("--watch and --background are mutually exclusive (--watch holds the foreground; --background detaches and returns)")
			}
			return runUp(cmd.Context(), opts)
		},
	}

	cmd.Flags().StringVar(&opts.env, "env", "", "Deploy environment to bring up (e.g. dev, staging) — required")
	cmd.Flags().BoolVar(&opts.noBuild, "no-build", false, "Skip the build phase (use already-built images / binaries)")
	cmd.Flags().BoolVar(&opts.noDeploy, "no-deploy", false, "Skip the cluster apply phase (host services and frontends still launch)")
	cmd.Flags().BoolVar(&opts.clusterOnly, "cluster-only", false, "Only run cluster phases (build + deploy); skip host/frontend")
	cmd.Flags().BoolVar(&opts.hostOnly, "host-only", false, "Only run host phases (host + frontend); skip build/deploy")
	cmd.Flags().BoolVar(&opts.background, "background", false, "Detach long-running phases and return immediately (stop with `forge up stop --env=<env>`). Beats --watch and the TTY default.")
	cmd.Flags().BoolVar(&opts.watch, "watch", false, "Force the hold-and-teardown lifecycle (block until Ctrl-C, then cascade-stop) even without a TTY. Default without --watch/--background: hold when stdin is a TTY, otherwise return after start (non-TTY agent/CI path).")
	cmd.Flags().BoolVar(&opts.restart, "restart", false, "If a stack is already running for this env, stop it (whole process tree) and bring it back up, instead of erroring on the port conflict")
	cmd.Flags().BoolVar(&opts.noGenerate, "no-generate", false, "Skip the pre-build code-generation check. By default `forge up` runs `forge generate` when gen/ is missing or proto sources are newer than the generated tree.")
	cmd.Flags().BoolVar(&opts.noInstall, "no-install", false, "Skip the pre-dev-serve frontend dependency install. By default `forge up` installs a frontend's deps when node_modules is missing or older than its lockfile/manifest.")
	cmd.Flags().StringArrayVar(&opts.targets, "target", nil, "Scope the host + frontend phases to specific services/frontends by name (repeatable). `forge up --target admin-server --host-only` is the single-service inner loop. Default: everything.")

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

// upDeployNamespace resolves the env's deploy namespace from the
// in-scope entities — the DEFAULT a KubeconfigSecret without its own
// namespace lands in. Order: the first cluster-service's declared
// namespace, then the manifests-only fallback (ManifestNamespace), then
// `<project>-<env>` (forge's namespace convention). Reads the entities
// already in hand rather than re-rendering KCL.
func upDeployNamespace(entities *KCLEntities, store projectstore.ProjectStore, env string) string {
	if entities != nil {
		for i := range entities.Services {
			s := &entities.Services[i]
			if s.Deploy.Type == "cluster" && s.Deploy.Cluster != nil && s.Deploy.Cluster.Namespace != "" {
				return s.Deploy.Cluster.Namespace
			}
		}
		if entities.ManifestNamespace != "" {
			return entities.ManifestNamespace
		}
	}
	if store != nil && env != "" {
		return store.Meta().Name + "-" + env
	}
	return ""
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
	portStorePath := filepath.Join(projectDir, ".forge", "ports-"+opts.env+".json")
	// Snapshot the store before render: RenderKCL's resolve_port shifts +
	// persists a port when the preferred one is busy (e.g. a second stack
	// is already up). If the already-running guard below then refuses this
	// run, we restore the snapshot so a rejected attempt can't drift the
	// stable port assignments. portStoreExisted distinguishes "restore the
	// old bytes" from "the store didn't exist; remove what render created".
	portStoreSnapshot, portStoreErr := os.ReadFile(portStorePath)
	portStoreExisted := portStoreErr == nil
	restorePortStore := func() {
		if portStoreExisted {
			_ = os.WriteFile(portStorePath, portStoreSnapshot, 0o644)
		} else {
			_ = os.Remove(portStorePath)
		}
	}
	kclplugin.UsePortStore(portStorePath)

	fmt.Printf("[up] env=%s\n", opts.env)
	entities, err := RenderKCL(ctx, projectDir, opts.env)
	if err != nil {
		return fmt.Errorf("render KCL: %w", err)
	}
	if entitiesEmpty(entities) {
		return fmt.Errorf("no services/operators/frontends/cronjobs declared in deploy/kcl/%s/", opts.env)
	}
	summarizeKCLBuildPlan(entities)

	// Derive this run's scope (which phases execute) from --cluster-only /
	// --host-only. scope is the single source of truth the phase gates below
	// read, instead of re-testing the raw flags at each site — and the seam
	// where a future entity kind (e.g. Terraform infra) becomes one more
	// scope field + gated block rather than another scattered conditional.
	scope := upScope(opts.clusterOnly, opts.hostOnly)

	// Build the per-env secret provider ONCE for this run (dotenv reads
	// the file now; external/none are cheap no-ops). Reused for both the
	// fail-fast validation below and the host-service env injection in
	// upHostServices — building it here avoids re-reading the dotenv per
	// service. When the host phase is in scope it WILL run, so validate up
	// front that every host service's declared secret_ref resolves against
	// the provider before any process starts. ValidateDeclaredRefs is a
	// no-op for external/none providers, so this only bites a dotenv
	// provider missing a declared key (and lists every miss at once).
	prov, err := secretProviderFromEntities(entities, projectDir)
	if err != nil {
		return fmt.Errorf("secret provider: %w", err)
	}
	if scope.host {
		dotenvPath := ""
		if entities.SecretProvider != nil {
			dotenvPath = entities.SecretProvider.Path
		}
		if err := secrets.ValidateDeclaredRefs(prov, secretRefsForHostServices(entities), dotenvPath); err != nil {
			return err // already actionable; lists every missing key
		}
	}

	// Cluster phases — build + deploy. Both are feature-gated: if the
	// project's forge.yaml turns either off (`features.build: false`
	// or `features.deploy: false`), the orchestrator skips the phase
	// with a one-line log and continues. Direct `forge build` /
	// `forge deploy` invocations still error — see requireFeature
	// in feature_gate.go for the strict-gate shape used by the cobra
	// RunE for those commands.
	if scope.cluster {
		// Cluster phase — ensure every declared k3d cluster exists BEFORE
		// anything builds or deploys (image pushes target a cluster's
		// registry; the deploy mounts Secrets into a cluster that must
		// already be up). Idempotent on warm runs (existing clusters are a
		// no-op). An env that declares no clusters (Bundle.clusters empty)
		// is a no-op here, preserving today's behavior. Skipped on
		// --no-deploy: with nothing to deploy there's no need to stand a
		// cluster up. Declared-cluster ensure is the multi-cluster
		// generalization of the dev-only ensureDevCluster on the deploy
		// path — ownership is implicit (Cluster.network / registry_mirror),
		// no "primary" cluster.
		if !opts.noDeploy && !skipFeature(store, config.FeatureDeploy, "up:clusters") {
			if err := reconcileDeclaredClusters(ctx, entities.Clusters, projectDir, opts.env); err != nil {
				return fmt.Errorf("clusters: %w", err)
			}
			// Mint cross-cluster kubeconfigs at the cluster→deploy
			// boundary: the clusters exist now (so `k3d kubeconfig get` +
			// the serverlb container are available), and the deploy phase
			// below hasn't rolled out the workloads that mount the Secret.
			// The in-network IP is resolved FRESH here and never persisted,
			// killing the IP-drift that a committed kubeconfig suffers when
			// a k3d cluster is recreated. No-op when none are declared.
			ownerNetwork := ownerNetworkFromClusters(entities.Clusters)
			deployNS := upDeployNamespace(entities, store, opts.env)
			if err := mintKubeconfigSecrets(ctx, entities.KubeconfigSecrets, ownerNetwork, deployNS); err != nil {
				return fmt.Errorf("kubeconfig secrets: %w", err)
			}
		}
		// Kick off the docker-compose infra (postgres/nats/temporal/...)
		// NOW, concurrent with the build phase. Image pulls + container
		// health warmup are the long pole on a warm `up` and are wholly
		// independent of the project image build, so overlapping the two
		// shaves that wall-clock off every run. The deploy phase below
		// re-dispatches the same compose group as an idempotent no-op once
		// warm (pull is current; `up -d` sees the containers already
		// running) and stays the authoritative health barrier before the
		// k8s rollout — so a best-effort failure here is non-fatal. Skipped
		// when the deploy phase won't run (--no-deploy): nothing would
		// consume the infra and nothing later barriers on it.
		var infraWarm chan error
		if scope.composeInfra && !opts.noDeploy && !skipFeature(store, config.FeatureDeploy, "up:infra") {
			infraWarm = make(chan error, 1)
			go func() {
				fmt.Println("\n[up] infra phase (compose — concurrent with build)")
				infraWarm <- prewarmComposeInfra(ctx, opts.env, entities)
			}()
		}
		if !opts.noBuild {
			if !skipFeature(store, config.FeatureBuild, "up:build") {
				fmt.Println("\n[up] build phase")
				if err := upBuildCluster(ctx, cfg, opts.env, opts.noGenerate); err != nil {
					return fmt.Errorf("build: %w", err)
				}
			}
		}
		// Barrier: the k8s pods the deploy phase rolls out connect to the
		// compose infra, so join the pre-warm before deploying. Joining here
		// (rather than letting the deploy phase's own compose-up race the
		// goroutine) also keeps a single docker-compose writer at a time.
		if infraWarm != nil {
			if err := <-infraWarm; err != nil {
				fmt.Printf("[up] infra pre-warm: %v (deploy phase will retry)\n", err)
			}
		}
		if !opts.noDeploy {
			if !skipFeature(store, config.FeatureDeploy, "up:deploy") {
				fmt.Println("\n[up] deploy phase")
				// Cluster reconcile through the SAME named entry point
				// `forge deploy` uses. `up`'s cluster step carries a
				// scope-derived (zero-value) deployOptions — no surgical
				// knobs today — instead of a blank `deployOptions{}` literal
				// standing in for "deploy with no options."
				if err := reconcileCluster(ctx, opts.env, deployOptions{}); err != nil {
					return fmt.Errorf("deploy: %w", err)
				}
			}
		}
	}

	// Nothing left to run once neither host nor frontend is in scope — the
	// --cluster-only case. Return after the apply; no processes to supervise.
	if !scope.host && !scope.frontend {
		fmt.Println("\n[up] --cluster-only: skipping host/frontend phases")
		return nil
	}

	// When the build phase was skipped (--host-only / --no-build) the
	// host runners (air / go-run) still compile against gen/, so ensure
	// generated code is present here too — otherwise host services fail
	// with the same "cannot load module gen" error the build phase would
	// have pre-empted. No-op when up-to-date or --no-generate. (The
	// non-skipped path already ran this inside runBuild.)
	if !scope.cluster || opts.noBuild {
		if err := ensureGeneratedCode(projectDirForKCL(), opts.noGenerate); err != nil {
			return fmt.Errorf("ensure generated code: %w", err)
		}
	}

	// Pre-flight "already running" guard. Before starting any host
	// process, check whether the ports the services/frontends THIS run
	// will start are already bound — a second stack on the same env
	// would otherwise silently shift resolve_port'd frontends to wrong
	// ports (persisting the wrong value) and hard-fail fixed-port host
	// services with "bind: address already in use". Respects --target
	// (only the services this invocation would start are checked) and
	// the frontend feature gate. Cluster-only never reaches here.
	frontendsOn := !skipFeature(store, config.FeatureFrontend, "up:frontend:portcheck")
	if conflicts := conflictingPorts(entities, opts.targets, frontendsOn, portInUse); len(conflicts) > 0 {
		// Tell OUR own already-running stack from a FOREIGN port holder via
		// the per-env lock (the tracked PIDs), not by guessing from the
		// bound port alone. Three outcomes:
		//   * our stack is live and --restart  -> stop it (tree-kill, which
		//     waits for the listeners to free) and continue;
		//   * our stack is live, no --restart  -> a clear error pointing at
		//     the unblock that actually works (forge up stop / --restart);
		//   * nothing tracked but the port is busy -> a foreign process;
		//     error pointing at lsof.
		ours := upStackLive(opts.env)
		if ours && opts.restart {
			fmt.Printf("\n[up] --restart: stopping the running env=%s stack first\n", opts.env)
			_ = runUpStop(opts.env) // tree-kill + wait-for-exit => ports released
			conflicts = conflictingPorts(entities, opts.targets, frontendsOn, portInUse)
		}
		if len(conflicts) > 0 {
			var b strings.Builder
			if ours && !opts.restart {
				fmt.Fprintf(&b, "[up] a forge stack is already running for env=%s:\n", opts.env)
				for _, c := range conflicts {
					fmt.Fprintf(&b, "       %-14s :%d\n", c.name, c.port)
				}
				fmt.Fprintf(&b, "     stop it:     forge up stop --env=%s\n", opts.env)
				fmt.Fprintf(&b, "     or refresh:  forge up --env=%s --restart", opts.env)
			} else {
				fmt.Fprintf(&b, "[up] these ports are held by a process forge doesn't manage (env=%s):\n", opts.env)
				for _, c := range conflicts {
					fmt.Fprintf(&b, "       %-14s :%d\n", c.name, c.port)
				}
				b.WriteString("     free them (lsof -i :<port>, kill the PID) or use --target to start a different service")
			}
			// Undo any resolve_port drift this rejected render persisted, so
			// the next clean run still gets the canonical port assignments.
			restorePortStore()
			return errors.New(b.String())
		}
	}

	// Resolve the lifecycle (Part B): supervise (hold + Ctrl-C teardown)
	// vs once (start, persist PIDs, return). `up`'s default is auto —
	// resolved here by the TTY check so an agent / CI `forge up` doesn't
	// hang on the interactive hold. --background and --watch override the
	// default (--background wins if somehow both set; rejected upstream).
	// `detach` collapses the lifecycle to the single behaviour the host
	// phase + summary need: a "once" run detaches every child (log files,
	// Process.Release, no foreground hold) so the stack OUTLIVES this
	// process, exactly as --background always has; a "supervise" run keeps
	// the live prefixed streams and holds. detach is true for BOTH the
	// explicit --background and the non-TTY default — they share the
	// return path verbatim.
	lifecycle := resolveUpLifecycle(cliutil.StdinIsTTY(), opts.watch, opts.background)
	detach := lifecycle == lifecycleOnce

	// Host phases — host services + frontends. These are tracked under
	// the orchestrator's child-process registry so Ctrl-C tears them
	// all down together.
	procs := newProcRegistry(opts.env)
	defer procs.shutdownOnExit()

	// Phase 3: host-mode services.
	if scope.host {
		hostFailures := upHostServices(ctx, cfg, entities, prov, opts.env, detach, opts.targets, procs)
		if hostFailures > 0 {
			fmt.Printf("[up] %d host service(s) failed to start (see above)\n", hostFailures)
		}
	}

	// Phase 4: frontends. In scope unless --cluster-only; further skipped
	// (with a log line) when features.frontend: false — the orchestrator
	// otherwise tries to npm-run-dev a tree that the project never scaffolded.
	if scope.frontend && !skipFeature(store, config.FeatureFrontend, "up:frontend") {
		feFailures := upFrontends(ctx, entities, opts.env, detach, opts.noInstall, opts.targets, procs)
		if feFailures > 0 {
			fmt.Printf("[up] %d frontend(s) failed to start (see above)\n", feFailures)
		}
	}

	// Summary box: what's listening where, and where to find each
	// service's log. Printed in both the supervise and detach paths so the
	// URLs + log paths are one glance away (and greppable for an agent).
	printUpSummary(entities, opts.env, detach, opts.targets)

	// Persist the per-env lock (tracked PIDs) for BOTH the supervise and
	// detach paths, so a concurrent `forge up` for this env detects the
	// running stack and `forge up stop` works. The supervise path removes
	// it again on shutdown (procs.shutdown / shutdownOnExit).
	procs.persist()

	if detach {
		fmt.Printf("[up] detached %d process(es). Stop with `forge up stop --env=%s`.\n",
			procs.count(), opts.env)
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
func printUpSummary(e *KCLEntities, env string, background bool, targets []string) {
	if e == nil {
		return
	}
	type row struct{ name, url, log string }
	var hosts, fronts []row
	for _, svc := range e.Services {
		if svc.Deploy.Type != "host" || svc.Deploy.Host == nil {
			continue
		}
		if !inTargetSet(targets, svc.Name) {
			continue
		}
		url := ""
		if p := hostEnvPort(svc.Name, svc.Deploy.Host); p != "" {
			url = "http://localhost:" + p
		}
		hosts = append(hosts, row{svc.Name, url, summaryLogPath(env, svc.Name)})
	}
	for _, fe := range e.Frontends {
		if !inTargetSet(targets, fe.Name) {
			continue
		}
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

// portConflict names a service/frontend the current `forge up` would
// start whose expected listen port is already bound by something else.
type portConflict struct {
	name string
	port int
}

// portInUse reports whether something is already listening on
// 127.0.0.1:<port>. A successful TCP dial within a short timeout means a
// listener is accepting connections; "connection refused" (the common
// free-port case) returns false. Used by the `forge up` pre-flight guard
// to refuse a colliding second stack before any process starts.
func portInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// conflictingPorts computes the set of services/frontends THIS `forge up`
// invocation would start whose expected listen port is already bound.
// It is the pure core of the pre-flight guard: probe is injected so the
// collection logic is testable without real sockets.
//
//   - Host services (deploy.Type=="host"): expected port via hostEnvPort
//     (skipped when the service declares no inline PORT).
//   - Frontends: fe.Port (skipped when 0, and entirely when frontendsOn
//     is false — the frontend feature is gated off).
//
// Only entities in the --target set are checked (inTargetSet), so
// `forge up --target reliant-web` next to a running admin-server is fine
// — the guard only fires for a service THIS run is about to start.
func conflictingPorts(e *KCLEntities, targets []string, frontendsOn bool, probe func(int) bool) []portConflict {
	if e == nil {
		return nil
	}
	var conflicts []portConflict
	for _, svc := range e.Services {
		if svc.Deploy.Type != "host" || svc.Deploy.Host == nil {
			continue
		}
		if !inTargetSet(targets, svc.Name) {
			continue
		}
		p := hostEnvPort(svc.Name, svc.Deploy.Host)
		if p == "" {
			continue
		}
		port, err := strconv.Atoi(p)
		if err != nil || port <= 0 {
			continue
		}
		if probe(port) {
			conflicts = append(conflicts, portConflict{name: svc.Name, port: port})
		}
	}
	if frontendsOn {
		for _, fe := range e.Frontends {
			if !inTargetSet(targets, fe.Name) {
				continue
			}
			if fe.Port <= 0 {
				continue
			}
			if probe(fe.Port) {
				conflicts = append(conflicts, portConflict{name: fe.Name, port: fe.Port})
			}
		}
	}
	return conflicts
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

// prewarmComposeInfra brings up the project's docker-compose deploy
// targets so their image pulls + health warmup can run concurrently with
// the build phase. It reuses the exact same path the deploy phase takes —
// buildDeployGroups → ComposeProvider.Deploy (pull + up -d + health
// check) — so the deploy phase's later re-dispatch is a true idempotent
// no-op, not a second, subtly-different code path. Namespace is irrelevant
// to compose targets, so the group builder gets an empty fallback. Returns
// the first compose group's error; the caller treats it as best-effort.
func prewarmComposeInfra(ctx context.Context, env string, entities *KCLEntities) error {
	groups, err := buildDeployGroups(env, entities, "")
	if err != nil {
		return fmt.Errorf("group compose services: %w", err)
	}
	provider := deploytarget.ComposeProvider{ProjectDir: projectDirForKCL()}
	for _, g := range groups {
		if g.ProviderID != provider.Name() {
			continue
		}
		if err := provider.Deploy(ctx, g); err != nil {
			return err
		}
	}
	return nil
}

// reconcileCluster is the cluster-scope reconcile entry point shared by
// `forge deploy` and `forge up`'s deploy phase: render the env's KCL,
// resolve context / namespace / secrets, and apply the in-cluster
// workloads + External/Compose deploy targets. opts carries the surgical
// knobs (tag / rollback / prune / dry-run / context override / targets /
// skip-frontend); `forge deploy` fills it from its flags, `forge up` passes
// the zero value (no knobs). Both reach the SAME pipeline — there is no
// longer a blank-`deployOptions{}` literal hiding the up-vs-deploy seam.
func reconcileCluster(ctx context.Context, env string, opts deployOptions) error {
	return runDeploy(ctx, env, opts)
}

// upHostServices starts every host-mode service as a child process,
// dispatching on deploy.Host.Runner. Returns the count of services that
// failed to start (logged but not fatal — the orchestrator brings up as
// many as it can rather than bailing on the first failure).
func upHostServices(ctx context.Context, cfg *config.ProjectConfig, e *KCLEntities, prov secrets.Provider, env string, background bool, targets []string, procs *procRegistry) int {
	// Resolve the secrets layer ONCE for the whole host phase. The
	// provider was built (and the dotenv read) once in runUp; All() is the
	// full per-env secret map for a dotenv provider, or nil for
	// external/none. buildHostServiceCmd layers this map (or the legacy
	// per-service secrets_file fallback) onto each service's env.
	secretsLayer := prov.All()
	failures := 0
	for _, svc := range e.Services {
		if svc.Deploy.Type != "host" || svc.Deploy.Host == nil {
			continue
		}
		if !inTargetSet(targets, svc.Name) {
			continue
		}
		cmd, name, err := buildHostServiceCmd(ctx, cfg, svc, secretsLayer, env)
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
// secrets → env_vars → os.Environ() wins last. See
// hostlaunch.LayerHostEnv for the full precedence rationale.
//
// secretsLayer is the per-env secret provider's resolved map (the dotenv
// provider's full map; nil for external/none). It is the authoritative
// secrets source when a provider is declared. When it is empty (no
// provider declared) the legacy per-service secrets_file is loaded as a
// backward-compat fallback so projects that haven't adopted a provider
// keep working.
//
// Unlike `forge run <svc>`, `forge up` is strict about unknown runners:
// a typo in KCL is fail-loud here because the orchestrator owns the
// whole environment and silent fallback to go-run could mask a deploy
// pin the user meant to apply. The hostlaunch package itself falls
// through to go-run on unknown runners; the explicit IsKnownRunner
// gate is what makes this call site strict.
func buildHostServiceCmd(ctx context.Context, cfg *config.ProjectConfig, svc ServiceEntity, secretsLayer map[string]string, env string) (*exec.Cmd, string, error) {
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
		// go-run target = the service's KCL GoBuild.cmd (the same package
		// `forge build` compiles), not a hardcoded ./cmd.
		GoRunCmd: goRunCmdForService(svc),
	}
	cmd := hostlaunch.BuildCmd(ctx, svc.Name, spec)

	// Env composition: projectConfig → secrets → env_vars →
	// os.Environ() wins last. Secrets are provider-first: the per-env
	// secret provider (built once in runUp, passed down as secretsLayer)
	// is the authoritative source when declared. Only when no provider is
	// declared (empty secretsLayer) do we fall back to the legacy
	// per-service secrets_file. A missing secrets_file is non-fatal
	// (warn-and-continue); parse / permission errors are fatal because
	// they signal a broken KCL pin rather than a developer who hasn't
	// created the file yet.
	secrets := secretsLayer
	if len(secrets) == 0 {
		loaded, lerr := hostlaunch.LoadSecretsFile(host.SecretsFile)
		switch {
		case lerr == nil:
			secrets = loaded
		case errors.Is(lerr, os.ErrNotExist):
			fmt.Printf("[up] host %s: warning: secrets file %s missing; continuing without it\n", svc.Name, host.SecretsFile)
		default:
			return nil, "", fmt.Errorf("host %s: read secrets file %s: %w", svc.Name, host.SecretsFile, lerr)
		}
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
func upFrontends(ctx context.Context, e *KCLEntities, env string, background, noInstall bool, targets []string, procs *procRegistry) int {
	failures := 0
	for _, fe := range e.Frontends {
		if !inTargetSet(targets, fe.Name) {
			continue
		}
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
	envFileMap, _ := envutil.ParseDotEnv(envFile)
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
// (un-prefixed) line there — the foreground file-tee.
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
		// Kill the whole process TREE so `go run`/Air's execed child — which
		// may have moved into its own process group — dies with the parent
		// instead of orphaning and squatting its port.
		killProcessTree(pid, syscall.SIGTERM)
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
			killProcessTree(pid, syscall.SIGKILL)
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
// trackedProc is one (name, pid) entry parsed from an env's lock file.
type trackedProc struct {
	name string
	pid  int
}

// trackedStack parses the env's lock file (~/.cache/forge/up/<env>.pids)
// into its (name, pid) entries. Returns nil when no lock is present. This
// is the single source of truth for "what did THIS env's `forge up` start".
func trackedStack(env string) []trackedProc {
	statePath, err := upStatePath(env)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil
	}
	var out []trackedProc
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
		// A non-positive pid is dropped: signalling 0/-1 is a footgun (on
		// Unix kill(-1) hits every process the user can reach).
		if _, err := fmt.Sscanf(parts[1], "%d", &pid); err != nil || pid <= 0 {
			continue
		}
		out = append(out, trackedProc{name: parts[0], pid: pid})
	}
	return out
}

// upStackLive reports whether the env's tracked stack has at least one live
// process. Lets the guard tell "re-running my own stack" from "a foreign
// process is on my port" deterministically, instead of guessing from a bound
// port. A lock whose PIDs are all dead is stale and treated as "not live".
func upStackLive(env string) bool {
	for _, t := range trackedStack(env) {
		if processAlive(t.pid) {
			return true
		}
	}
	return false
}

func runUpStop(env string) error {
	statePath, err := upStatePath(env)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(statePath); statErr != nil {
		if os.IsNotExist(statErr) {
			fmt.Printf("[up] no tracked stack for env=%s.\n", env)
			return nil
		}
		return fmt.Errorf("stat state: %w", statErr)
	}
	procs := trackedStack(env)

	// SIGTERM each process TREE — the runner plus every transitive
	// descendant, including the server a runner like Air re-execs in its own
	// process group. (Signalling only the runner's group left that respawned
	// child orphaned and squatting its port — the bug we're fixing.)
	for _, t := range procs {
		fmt.Printf("[up] %s: stopping (pid %d + tree)\n", t.name, t.pid)
		killProcessTree(t.pid, syscall.SIGTERM)
	}

	// Wait for the runners to actually exit by POLLING liveness, so a caller
	// like `--restart` knows the listeners are released on return. Return the
	// instant they're gone; SIGKILL stragglers after a bounded grace.
	deadline := time.Now().Add(8 * time.Second)
	for {
		anyAlive := false
		for _, t := range procs {
			if processAlive(t.pid) {
				anyAlive = true
				break
			}
		}
		if !anyAlive || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, t := range procs {
		if processAlive(t.pid) {
			fmt.Printf("[up] %s: did not exit, SIGKILL\n", t.name)
			killProcessTree(t.pid, syscall.SIGKILL)
		}
	}

	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("[up] warning: remove state: %v\n", err)
	}
	fmt.Printf("[up] stopped %d process(es) for env=%s.\n", len(procs), env)
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
