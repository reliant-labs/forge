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
	"encoding/json"
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
	cmd.AddCommand(newUpServicesCmd())
	return cmd
}

// newUpServicesCmd is the retrieve-after-the-fact half of the `forge up`
// summary: `forge up services --env=<env>` re-derives the same host
// service + frontend table long after the startup scrollback has scrolled
// away, so a human (or an agent that reconnected to a running stack) can
// re-discover every listening URL, its log file, and whether it's actually
// up — without re-running `forge up`. It renders the env's KCL through the
// SAME devstack context `forge up` uses (identical ports), probes each
// declared port for a live listener, and cross-references the ownership
// markers the reclaim guard stamps so it can tell "our process is up" from
// "something else grabbed the port".
func newUpServicesCmd() *cobra.Command {
	var env string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "services",
		Short: "List the host services + frontends `forge up` runs for an env — URLs, log paths, and live listen status",
		Long: `List every host service and frontend the env's ` + "`forge up`" + ` runs, with:

  * its browser URL (http://localhost:<port>),
  * its per-service log file (tail -f / grep target), and
  * whether a listener is accepting on the port RIGHT NOW (up/down),
    including the holder pid and whether that process is forge-owned.

Reads the same rendered KCL + resolved ports ` + "`forge up`" + ` uses, so the
table matches what ` + "`forge up --env=<env>`" + ` printed — retrievable after
the startup scrollback is gone. ALL declared frontends are listed (a
project may declare several; each gets its own port row).

Examples:
  forge up services --env=dev
  forge up services --env=dev --json   # machine-readable for scripts/agents`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if env == "" {
				return fmt.Errorf("--env is required (e.g. --env=dev)")
			}
			return runUpServices(cmd.Context(), env, jsonOut)
		},
	}
	cmd.Flags().StringVar(&env, "env", "", "Environment to report (e.g. dev) — required")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON (name/kind/url/port/log/listening/pid/owned per service)")
	return cmd
}

// runUpServices renders the env's KCL, probes each declared port, and
// prints the host-service + frontend table (or JSON). It arms the SAME
// devstack render context `forge up`/`forge deploy` arm so ports resolve
// identically, then restores any resolve_port drift — this is a read-only
// report and must not shift the stable port assignments a live stack is on.
func runUpServices(ctx context.Context, env string, jsonOut bool) error {
	store, err := loadProjectStore()
	if err != nil {
		return err
	}
	projectDir := projectDirForKCL()
	_, restore := activateDevStack(projectDir, env)
	entities, err := RenderKCL(ctx, projectDir, env)
	restore() // revert resolve_port bytes; a status render must not drift ports
	if err != nil {
		return fmt.Errorf("render KCL: %w", err)
	}

	// Honor the frontend feature gate so a frontends-off project's report
	// doesn't list frontends `forge up` never starts. Use the pure predicate
	// (not skipFeature) so this read-only report emits no phase-skip log line
	// — that would pollute the --json output and is meaningless here (the
	// `services` command runs no phase).
	frontendsOn := isFeatureEnabled(store, config.FeatureFrontend)
	rows := collectUpServices(entities, env, nil, frontendsOn, portInUse)
	// Enrich with the listener pid + forge-ownership marker — the reclaim
	// guard's signal, reused here to distinguish "our stack is up" from "a
	// foreign process holds the port".
	enrichOwnership(rows, env)

	if jsonOut {
		rep := upServicesReport{Env: env, Services: rows}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if len(rows) == 0 {
		fmt.Printf("[up] no host services or frontends declared in deploy/kcl/%s/\n", env)
		return nil
	}
	// notReadyLabel "down": unlike the immediate post-launch summary (where a
	// not-yet-listening port means "still booting"), this snapshot runs long
	// after start, so a dead port is genuinely down.
	renderUpSummary(os.Stdout, env, rows, "down", true, nil)
	return nil
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
// metaReader is the one-method slice of the project store this namespace
// resolver needs — declared at the consumer so the helper depends on
// `Meta()` alone, not the store's full surface. *projectstore.Store
// satisfies it.
type metaReader interface {
	Meta() projectstore.ProjectMeta
}

func upDeployNamespace(entities *KCLEntities, store metaReader, env string) string {
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

	// Arm the parallel-dev-stack render context: push the raw git facts
	// option("worktree")/option("branch") into KCL, back forge.allocate_port
	// with the lock-guarded block registry, and activate the resolve_port
	// store. `forge deploy` arms the SAME inputs, so up and deploy resolve
	// identical ports — no drift. State is machine-local (.forge/blocks.json,
	// .forge/ports-*.json are gitignored). restorePortStore reverts the
	// resolve_port store if the already-running guard below rejects this
	// render (a rejected attempt must not drift the stable assignments).
	_, restorePortStore := activateDevStack(projectDir, opts.env)

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
		if err := upClusterBringup(ctx, upClusterInput{
			store: store, cfg: cfg, entities: entities, projectDir: projectDir,
			opts: opts, scope: scope,
		}); err != nil {
			return err
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
		// Tell OUR own (possibly orphaned) stack from a FOREIGN port holder by
		// INSPECTING THE LIVE HOLDER, not by guessing from the bound port and
		// not by trusting the drift-prone .pids ledger alone. A process
		// carrying our FORGE_UP_ENV marker — on the holder or any ancestor —
		// is our orphan even when the ledger is stale/absent (a crash mid-run,
		// air re-exec under a new pid, or an npm grandchild reparented to
		// launchd). classifyPortConflicts splits the conflicts accordingly;
		// the ledger is still consulted as a fast "is a live stack tracked"
		// hint. Outcomes:
		//   * ours (marker or ledger) and --restart -> reclaim it (tree-kill,
		//     which waits for the listeners to free) and continue;
		//   * ours, no --restart -> a clear error pointing at the unblock that
		//     actually works (forge up --restart / forge up stop);
		//   * genuinely foreign (unmarked holder) -> error pointing at lsof.
		//     A foreign process is NEVER killed.
		facts := newOSProcFacts()
		owned, foreign := classifyPortConflicts(opts.env, conflicts, portListenerPID, facts)
		ours := upStackLive(opts.env) || len(owned) > 0

		if opts.restart && ours {
			fmt.Printf("\n[up] --restart: reclaiming the running/orphaned env=%s stack first\n", opts.env)
			if upStackLive(opts.env) {
				_ = runUpStop(opts.env) // tree-kill ledger pids + wait-for-exit
			}
			// Reclaim marker-identified orphans the ledger never recorded (the
			// common case after a crash: real forge processes, no live ledger).
			reclaimMarkedOrphans(opts.env, conflicts, portListenerPID, facts)
			conflicts = conflictingPorts(entities, opts.targets, frontendsOn, portInUse)
			// Re-classify against a FRESH process snapshot — the reclaim above
			// changed the table, so the pre-reclaim ppid map is stale.
			owned, foreign = classifyPortConflicts(opts.env, conflicts, portListenerPID, newOSProcFacts())
		}

		if len(conflicts) > 0 {
			var b strings.Builder
			// Our own (possibly orphaned) stack still holds ports — route to
			// the "ours" message even when the ledger said nothing, because
			// the live holder carries our marker.
			if len(owned) > 0 {
				_, _ = fmt.Fprintf(&b, "[up] a forge-managed stack for env=%s is still running (possibly orphaned from an earlier run):\n", opts.env)
				for _, c := range owned {
					_, _ = fmt.Fprintf(&b, "       %-14s :%d\n", c.name, c.port)
				}
				_, _ = fmt.Fprintf(&b, "     refresh:  forge up --env=%s --restart\n", opts.env)
				_, _ = fmt.Fprintf(&b, "     or stop:  forge up stop --env=%s", opts.env)
			}
			if len(foreign) > 0 {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				_, _ = fmt.Fprintf(&b, "[up] these ports are held by a process forge doesn't manage (env=%s):\n", opts.env)
				for _, c := range foreign {
					_, _ = fmt.Fprintf(&b, "       %-14s :%d\n", c.name, c.port)
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
	// frontendsOn (computed above for the port-conflict guard) is reused so
	// the summary lists exactly the frontends this run actually started.
	printUpSummary(entities, opts.env, detach, opts.targets, frontendsOn)

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

// upClusterInput carries the inputs upClusterBringup needs for the cluster
// phase (cluster ensure + kubeconfig secrets + concurrent compose infra +
// build + deploy).
type upClusterInput struct {
	store      *projectstore.Store
	cfg        *config.ProjectConfig
	entities   *KCLEntities
	projectDir string
	opts       upOptions
	scope      reconcileScope
}

// upClusterBringup runs `forge up`'s cluster phases: ensure every declared k3d
// cluster exists (and mint cross-cluster kubeconfig secrets) BEFORE anything
// builds or deploys, kick off the docker-compose infra concurrently with the
// build, run the build phase, barrier on the infra pre-warm, then run the
// deploy phase. Both build and deploy are feature-gated (features.build /
// features.deploy off → skip with a one-line log). All cluster-ensure/deploy
// work is skipped on --no-deploy; the build phase is skipped on --no-build.
func upClusterBringup(ctx context.Context, in upClusterInput) error {
	store, cfg, entities, opts, scope := in.store, in.cfg, in.entities, in.opts, in.scope
	// Cluster phase — ensure every declared k3d cluster exists BEFORE
	// anything builds or deploys (image pushes target a cluster's
	// registry; the deploy mounts Secrets into a cluster that must
	// already be up). Idempotent on warm runs (existing clusters are a
	// no-op). An env that declares no clusters (Bundle.clusters empty)
	// is a no-op here, preserving today's behavior. Skipped on
	// --no-deploy: with nothing to deploy there's no need to stand a
	// cluster up. Declared-cluster ensure is the multi-cluster
	// generalization of the dev-only ensureDevCluster on the deploy
	// path — ownership is a reference (Cluster.owner drives the derived
	// network / registry-inherit), no "primary" cluster.
	if !opts.noDeploy && !skipFeature(store, config.FeatureDeploy, "up:clusters") {
		if err := reconcileDeclaredClusters(ctx, entities.Clusters, in.projectDir, opts.env); err != nil {
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
			// scope-derived deployOptions — instead of a blank
			// `deployOptions{}` literal standing in for "deploy with no
			// options."
			//
			// skipFrontend: true is the deploy-phase mirror of the build
			// phase's skipFrontends (upBuildCluster). `forge up` ALWAYS
			// dev-serves frontends in its Phase 4 (`npm run dev` =
			// `next dev` / vite dev server) and NEVER consumes a prod
			// frontend artifact, so the deploy phase must not run the
			// `deploy = None` build-only path (dispatchFrontendDeploys →
			// `npm run build` under NODE_ENV=production). Without this a
			// bare `forge up --env=dev` would prod-`next build` every
			// host-mode frontend (the static `output: "export"` Next.js
			// build) right before — and pointlessly alongside — starting
			// its `next dev` server. The build-only path exists to
			// materialize a static frontend for a FirebaseHosting frontend
			// to reference at DEPLOY time; it has no place in the dev loop.
			if err := reconcileCluster(ctx, opts.env, deployOptions{skipFrontend: true}); err != nil {
				return fmt.Errorf("deploy: %w", err)
			}
		}
	}
	return nil
}

// upServiceRow is one host service / frontend line shared by the immediate
// `forge up` summary and the retrieve-after-the-fact `forge up services`
// output. The JSON tags are the `--json` contract (kept additive so
// dashboards/agents stay stable as fields are added).
type upServiceRow struct {
	Name string `json:"name"`
	Kind string `json:"kind"`           // "host" | "frontend"
	URL  string `json:"url,omitempty"`  // browser-reachable URL; empty when no port is declared
	Port int    `json:"port,omitempty"` // declared listen port; 0 when unknown
	Log  string `json:"log"`            // project-relative log path (tail/grep target)
	// Listening is a point-in-time port probe: true when a TCP listener is
	// accepting on Port. False for a not-yet-bound (booting) or dead service,
	// and always false when Port is 0.
	Listening bool `json:"listening"`
	// PID / Owned are filled by enrichOwnership (the `services` command path):
	// the pid LISTENing on Port, and whether it — or an ancestor — carries
	// this env's forge-up ownership marker. PID 0 / Owned false when no
	// listener, or on platforms where the port→pid lookup is a no-op.
	PID   int  `json:"pid,omitempty"`
	Owned bool `json:"owned,omitempty"`
}

// upServicesReport is the `forge up services --json` envelope. Stable so
// scripts / sub-agents can consume it.
type upServicesReport struct {
	Env      string         `json:"env"`
	Services []upServiceRow `json:"services"`
}

// collectUpServices builds the ordered host-then-frontend rows for env,
// scoped by --target (inTargetSet) and — for frontends — the frontendsOn
// gate. It is the pure core shared by the immediate summary and the
// `services` command: the port liveness probe is injected so the
// collection is unit-testable without real sockets (pass nil to skip it).
//
// Host-service ports come from the KCL PORT convention (hostEnvPort); a
// service declaring no inline PORT is listed without a URL. EVERY declared
// frontend is emitted (a project may declare several, each with its own
// port) — never collapsed to one.
func collectUpServices(e *KCLEntities, env string, targets []string, frontendsOn bool, probe func(int) bool) []upServiceRow {
	if e == nil {
		return nil
	}
	var rows []upServiceRow
	for _, svc := range e.Services {
		if svc.Deploy.Type != "host" || svc.Deploy.Host == nil {
			continue
		}
		if !inTargetSet(targets, svc.Name) {
			continue
		}
		r := upServiceRow{Name: svc.Name, Kind: "host", Log: summaryLogPath(env, svc.Name)}
		if p := hostEnvPort(svc.Name, svc.Deploy.Host); p != "" {
			if port, err := strconv.Atoi(p); err == nil && port > 0 {
				r.Port = port
				r.URL = "http://localhost:" + p
			}
		}
		rows = append(rows, r)
	}
	if frontendsOn {
		for _, fe := range e.Frontends {
			if !inTargetSet(targets, fe.Name) {
				continue
			}
			r := upServiceRow{Name: fe.Name, Kind: "frontend", Log: summaryLogPath(env, "frontend:"+fe.Name)}
			if fe.Port > 0 {
				r.Port = fe.Port
				r.URL = fmt.Sprintf("http://localhost:%d", fe.Port)
			}
			rows = append(rows, r)
		}
	}
	if probe != nil {
		probeRowsListening(rows, probe)
	}
	return rows
}

// probeRowsListening fills each row's Listening flag by probing its Port
// CONCURRENTLY, so the liveness snapshot costs one dial timeout total
// rather than one-per-service (matters when several services are still
// booting and each dial pays the full timeout). Rows without a known port
// are left false.
func probeRowsListening(rows []upServiceRow, probe func(int) bool) {
	var wg sync.WaitGroup
	for i := range rows {
		if rows[i].Port <= 0 {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rows[i].Listening = probe(rows[i].Port)
		}(i)
	}
	wg.Wait()
}

// enrichOwnership fills each listening row's PID + Owned by resolving the
// port's listener (portListenerPID / lsof) and walking its ancestry for
// this env's forge-up marker (forgeOwnerOfPID). It CONSUMES the ownership
// work's helpers rather than re-discovering process ownership — the same
// signal the pre-flight reclaim guard uses. Best-effort: on platforms
// where portListenerPID is a no-op (Windows) PID stays 0 and Owned false,
// so the report degrades to health-only without misfiring.
func enrichOwnership(rows []upServiceRow, env string) {
	facts := newOSProcFacts()
	for i := range rows {
		if rows[i].Port <= 0 {
			continue
		}
		pid := portListenerPID(rows[i].Port)
		if pid <= 0 {
			continue
		}
		rows[i].PID = pid
		if _, ok := forgeOwnerOfPID(pid, env, facts); ok {
			rows[i].Owned = true
		}
	}
}

// printUpSummary prints a compact box of what `forge up` just brought
// up: each host service and frontend, its URL (when a listen port is
// known), a point-in-time listen check, and the path to its log file —
// plus where to grep all logs, how to list cluster routes, and how to
// re-list this table later. Mirrors the cloud-dev "final banner" so a
// developer (or an LLM agent) can find URLs and logs at a glance instead
// of scraping them out of interleaved startup scrollback.
//
// Health here is best-effort: the processes JUST started, so a service
// still binding its port shows "starting" (not "down"). Use
// `forge up services --env=<env>` for the settled status.
func printUpSummary(e *KCLEntities, env string, background bool, targets []string, frontendsOn bool) {
	rows := collectUpServices(e, env, targets, frontendsOn, portInUse)
	if len(rows) == 0 {
		return
	}
	trailer := "Ctrl-C to stop."
	if background {
		trailer = fmt.Sprintf("Detached — stop with `forge up stop --env=%s`", env)
	}
	// showOwner=false: the immediate summary stays off the lsof/ps hot path;
	// notReadyLabel="starting" because a just-launched port not yet bound is
	// booting, not down.
	renderUpSummary(os.Stdout, env, rows, "starting", false, []string{trailer})
}

// renderUpSummary writes the aligned host-service + frontend table + the
// standard "where to look next" footer (logs dir, cluster routes, live
// status command) to w. Shared by the immediate summary and the `services`
// command so both print the identical block.
//
//   - notReadyLabel is the status word for a known-but-not-listening port
//     ("starting" right after launch, "down" for the settled snapshot).
//   - showOwner appends the listener pid + a "not forge-owned" flag when a
//     foreign process holds the port (the `services` command).
//   - trailers are extra footer lines (e.g. the Ctrl-C / detached hint).
func renderUpSummary(w io.Writer, env string, rows []upServiceRow, notReadyLabel string, showOwner bool, trailers []string) {
	if len(rows) == 0 {
		return
	}
	// Column widths from the data so name/URL/status align in one table.
	nameW, urlW := 0, 0
	for _, r := range rows {
		if len(r.Name) > nameW {
			nameW = len(r.Name)
		}
		u := r.URL
		if u == "" {
			u = summaryNoPort
		}
		if len(u) > urlW {
			urlW = len(u)
		}
	}

	const bar = "│"
	printRow := func(r upServiceRow) {
		urlCell := r.URL
		if urlCell == "" {
			urlCell = summaryNoPort
		}
		fmt.Fprintf(w, "%s  %s %-*s  %-*s  %s\n",
			bar, statusGlyph(r, notReadyLabel), nameW, r.Name, urlW, urlCell, rowStatus(r, notReadyLabel, showOwner))
		fmt.Fprintf(w, "%s       ↳ %s\n", bar, r.Log)
	}
	printGroup := func(title, kind string) {
		printed := false
		for _, r := range rows {
			if r.Kind != kind {
				continue
			}
			if !printed {
				fmt.Fprintf(w, "%s %s\n", bar, title)
				printed = true
			}
			printRow(r)
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "╭─ forge up · env=%s ─────────────────────────────────────\n", env)
	printGroup("Host services", "host")
	printGroup("Frontends", "frontend")
	fmt.Fprintf(w, "%s\n", bar)
	fmt.Fprintf(w, "%s Logs   %s/   — tail -f / grep the per-service *.log here\n", bar, upLogDir(env))
	fmt.Fprintf(w, "%s Cluster routes:  forge cluster urls\n", bar)
	fmt.Fprintf(w, "%s Live status:     forge up services --env=%s\n", bar, env)
	for _, t := range trailers {
		fmt.Fprintf(w, "%s %s\n", bar, t)
	}
	fmt.Fprintln(w, "╰─────────────────────────────────────────────────────────")
	fmt.Fprintln(w)
}

// summaryNoPort is the URL cell for a service that declares no listen port
// (nothing browser-reachable to link).
const summaryNoPort = "(no port declared)"

// statusGlyph is the leading health mark for a summary row: a filled dot
// when a listener is accepting, a hollow dot for a known-but-not-listening
// port, and a middot when no port is declared (health is not meaningful).
func statusGlyph(r upServiceRow, _ string) string {
	switch {
	case r.Port <= 0:
		return "·"
	case r.Listening:
		return "●"
	default:
		return "○"
	}
}

// rowStatus is the trailing status word for a summary row. With showOwner
// it annotates a live port with its holder pid and flags a listener that
// is NOT forge-owned (something else grabbed the port) — the ownership
// signal reused from the reclaim guard.
func rowStatus(r upServiceRow, notReadyLabel string, showOwner bool) string {
	if r.Port <= 0 {
		return ""
	}
	if !r.Listening {
		return notReadyLabel
	}
	if !showOwner || r.PID <= 0 {
		return "up"
	}
	if r.Owned {
		return fmt.Sprintf("up (pid %d)", r.PID)
	}
	return fmt.Sprintf("up (pid %d, not forge-owned)", r.PID)
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
	dialer := net.Dialer{Timeout: 300 * time.Millisecond}
	conn, err := dialer.DialContext(context.Background(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
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
	secretVals := secretsLayer
	if len(secretVals) == 0 {
		loaded, lerr := hostlaunch.LoadSecretsFile(host.SecretsFile)
		switch {
		case lerr == nil:
			secretVals = loaded
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
	cmd.Env = hostlaunch.LayerHostEnv(os.Environ(), projectConfigEnv, secretVals, envVars)

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
	// Stamp forge ownership onto the child (and, via env inheritance, every
	// descendant) BEFORE it starts. This is the authoritative signal a later
	// `forge up` uses to recognise its own orphans on a busy port even when
	// the .pids ledger has drifted — see up_reclaim.go.
	stampForgeOwnership(cmd, p.env, name)
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
			_, _ = fmt.Fprintln(logSink, line)
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
		_, _ = fmt.Fprintf(&b, "%s\t%d\n", mp.name, pid)
	}
	p.mu.Unlock()
	// Write atomically (temp + rename) so a crash mid-write can't leave a
	// truncated/corrupt ledger that `forge up stop` would misparse. The temp
	// lives in the same dir so the rename is a same-filesystem atomic swap.
	tmp := statePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		fmt.Printf("[up] warning: write state: %v\n", err)
		return
	}
	if err := os.Rename(tmp, statePath); err != nil {
		fmt.Printf("[up] warning: commit state: %v\n", err)
		_ = os.Remove(tmp)
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
	// The ledger is a fast index of what THIS env's `forge up` started, but
	// it drifts (a crashed run never persisted; air re-execs under a new
	// pid). It is no longer the sole source of truth: after tearing down the
	// tracked pids we ALSO sweep the process table for anything still
	// carrying our FORGE_UP_ENV marker, so `forge up stop` is always a clean
	// unblock even when the ledger is stale or absent.
	procs := trackedStack(env) // nil when no ledger present

	// SIGTERM each tracked process TREE — the runner plus every transitive
	// descendant, including the server a runner like Air re-execs in its own
	// process group. (Signalling only the runner's group left that respawned
	// child orphaned and squatting its port — the bug we're fixing.)
	ledgerPIDs := make([]int, 0, len(procs))
	for _, t := range procs {
		fmt.Printf("[up] %s: stopping (pid %d + tree)\n", t.name, t.pid)
		ledgerPIDs = append(ledgerPIDs, t.pid)
	}
	// killTreesAndWait SIGTERMs, polls liveness (so a `--restart` caller
	// knows the listeners are released on return), then SIGKILLs stragglers.
	killTreesAndWait(ledgerPIDs)

	// Marker sweep: reclaim any forge-owned orphan for this env the ledger
	// missed. Runs unconditionally (even with no ledger) so a wedged env
	// always has a clean unblock. Only processes carrying the exact env
	// marker are ever signalled — an unmarked process is never touched.
	reclaimed := reclaimAllMarkedOrphans(env, newOSProcFacts())

	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("[up] warning: remove state: %v\n", err)
	}
	total := len(procs) + reclaimed
	if total == 0 {
		fmt.Printf("[up] no tracked stack for env=%s.\n", env)
		return nil
	}
	fmt.Printf("[up] stopped %d process(es) for env=%s.\n", total, env)
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
