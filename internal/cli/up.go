// Package cli — `forge env up <env>` orchestrator.
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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/doctor"
	"github.com/reliant-labs/forge/internal/envutil"
	"github.com/reliant-labs/forge/internal/hostinfra"
	"github.com/reliant-labs/forge/internal/hostlaunch"
	"github.com/reliant-labs/forge/internal/projectstore"
	"github.com/reliant-labs/forge/internal/secrets"
)

// upOptions bundles flags for `forge env up`.
type upOptions struct {
	env        string
	noBuild    bool
	noDeploy   bool
	background bool // detach and write PID files; use `forge env down <env>` to teardown
	watch      bool // force supervise (hold + Ctrl-C teardown) even without a TTY
	noGenerate bool // skip the pre-build "ensure generated code" step (--no-generate)
	noInstall  bool // skip the pre-dev-serve "ensure frontend deps" step (--no-install)
	noSeed     bool // skip the first-boot dev auto-seed (--no-seed)
	// targets, when non-empty, scopes the WHOLE run to the named
	// services/operators/frontends — build, deploy, host and frontend
	// phases alike, mirroring `forge env deploy --target`. Naming a
	// frontend therefore no longer docker-builds and pushes every cluster
	// service on the way to starting one dev server. Empty means
	// "everything", the default.
	targets []string
	// renderOptions are raw `-D name=value` values pushed into the env's KCL
	// as top-level options. OPAQUE to forge: it validates the name against
	// what the env declares, relays the value verbatim, and never interprets
	// either. See internal/cli/up_options.go.
	renderOptions []string
	// frontendArgs are passthrough tokens forwarded to each frontend's dev
	// server command (`npm run dev -- <frontendArgs>`). Not bound to a
	// `forge env up` flag — it's the seam `forge run -- <flags>` sets so the
	// reliant one-shot's `forge run -- --host 0.0.0.0` reaches Vite. Empty
	// (the default, and always for `forge env up`) is a no-op.
	frontendArgs []string
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

func newEnvUpCmd() *cobra.Command {
	var opts upOptions

	cmd := &cobra.Command{
		Use:   "up <environment>",
		Short: "Bring the whole dev loop up: build + deploy + host + frontend",
		Args:  cobra.ExactArgs(1),
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

Use --no-build / --no-deploy to skip phases when iterating. Use --target
<name> to scope the whole run to one service — a CI lane that only wants
that service's cluster apply, or a dev loop iterating on one host-mode
service, both scope the same way.

Lifecycle (what happens after host services + frontends start):

  * With a TTY (interactive shell): forge holds the foreground and
    tears the whole stack down on Ctrl-C.
  * Without a TTY (agent / CI / piped): forge brings everything up,
    prints the summary (URLs + per-service log paths), and RETURNS,
    leaving the processes running — the same end-state as --background.
    This is what keeps ` + "`forge env up <env>`" + ` from hanging an agent.
  * --watch forces the hold-and-teardown lifecycle even without a TTY
    (for a human piping the output through a tool).
  * --background always detaches and returns immediately, regardless of
    the TTY. If both --watch and --background are passed, --background
    wins (detach + return).

Either way the long-running children are tracked under
~/.cache/forge/up/<project-id>/; stop a detached / non-TTY stack with
` + "`forge env down <env>`" + `.

ONE stack per (project, env). If this project already has a stack running
for this env — tracked, detached, or orphaned by a crashed run — it is
STOPPED before the new one starts. It is not adopted: this invocation may
carry different config, a different allocated port, or reinstalled deps, so
the old process is not the process you asked for. Only processes carrying
forge's own ownership markers for THIS project and env are ever signalled;
a port held by anything else is an error, never a kill.

Examples:
  forge env up dev
  forge env up dev --no-build
  forge env up dev --target admin-server -D host_runner=go-run
  forge env up dev --watch        # hold + Ctrl-C teardown even when piped
  forge env up dev --background
  forge env down dev

Render options (-D):
  An env's KCL can declare options for the things you want to vary per run —
  which runner a host service launches under, whether to point at a remote
  dependency, anything else the env models. It declares one by reading it:

    _host_runner = option("host_runner", type="str", default="air",
                          help="Host launch runner: air (default) or go-run")

  and you set it with:

    forge env up dev -D host_runner=go-run

  forge does not interpret these. It checks the NAME against what the env
  declares (so a typo fails instead of silently doing nothing), relays the
  value verbatim, and the KCL decides what it means. List what an env
  declares with ` + "`forge env options <env>`" + `.

  Options forge derives itself (env, namespace, image_tag, image_digests,
  worktree, branch) are not yours to set and are rejected. -D is accepted on
  ` + "`env up`" + ` only — a cluster apply must stay reproducible from the repo alone.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.env = args[0]
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

	cmd.Flags().BoolVar(&opts.noBuild, "no-build", false, "Skip the build phase (use already-built images / binaries)")
	cmd.Flags().BoolVar(&opts.noDeploy, "no-deploy", false, "Skip the cluster apply phase (host services and frontends still launch)")
	cmd.Flags().BoolVar(&opts.background, "background", false, "Detach long-running phases and return immediately (stop with `forge env down <env>`). Beats --watch and the TTY default.")
	cmd.Flags().BoolVar(&opts.watch, "watch", false, "Force the hold-and-teardown lifecycle (block until Ctrl-C, then cascade-stop) even without a TTY. Default without --watch/--background: hold when stdin is a TTY, otherwise return after start (non-TTY agent/CI path).")
	cmd.Flags().BoolVar(&opts.noGenerate, "no-generate", false, "Skip the pre-build code-generation check. By default `forge env up` runs `forge generate` when gen/ is missing or proto sources are newer than the generated tree.")
	cmd.Flags().BoolVar(&opts.noInstall, "no-install", false, "Skip the pre-dev-serve frontend dependency install. By default `forge env up` installs a frontend's deps when node_modules is missing or older than its lockfile/manifest.")
	cmd.Flags().BoolVar(&opts.noSeed, "no-seed", false, "Skip the first-boot dev auto-seed. By default `forge run`/`forge env up` seeds a dev database once when it is reachable and all seedable tables are empty.")
	cmd.Flags().StringArrayVar(&opts.targets, "target", nil, "Scope the whole run — build, deploy, host and frontend phases — to specific services/operators/frontends by name (repeatable). Targeting only host/frontend apps builds no images. An unknown name is an error listing the env's app names. Default: everything.")
	cmd.Flags().StringArrayVarP(&opts.renderOptions, "option", "D", nil, "Set a render option the env's KCL declares, as name=value (repeatable). Relayed to KCL verbatim — forge does not interpret the value. List an env's options with `forge env options <env>`.")

	return cmd
}

// newEnvStatusCmd is the retrieve-after-the-fact half of the `forge env up`
// summary: `forge env status <env>` re-derives the same host
// service + frontend table long after the startup scrollback has scrolled
// away, so a human (or an agent that reconnected to a running stack) can
// re-discover every listening URL, its log file, and whether it's actually
// up — without re-running `forge env up`. It renders the env's KCL through
// the SAME devstack context `forge env up` uses (identical ports), probes
// each declared port for a live listener, and cross-references the ownership
// markers the reclaim guard stamps so it can tell "our process is up" from
// "something else grabbed the port".
func newEnvStatusCmd() *cobra.Command {
	var (
		jsonOut bool
		signal  string
		verbose bool
	)
	cmd := &cobra.Command{
		Use:   "status <environment>",
		Short: "Everything runtime about an env: host services, frontends, compose infra, app health, telemetry",
		Args:  cobra.ExactArgs(1),
		Long: `Report the runtime state of an environment.

The TABLE lists every host service and frontend the env's ` + "`forge env up`" + `
runs, with:

  * its browser URL (http://localhost:<port>),
  * its per-service log file (tail -f / grep target),
  * whether a listener is accepting on the port RIGHT NOW (up/down),
    including the holder pid and whether that process is forge-owned,
  * for each host service, the live SERVER process(es) backing it with
    build-freshness — binary path, its build/mtime, the process start
    time, and whether it is stale vs the repo HEAD commit, and
  * a loud DUPLICATE flag when more than one process is serving the same
    host service (the "air spawned a new worker but didn't reap the old
    one" symptom — stale and fresh build vintages running at once).

The RUNTIME CHECKS underneath probe the rest of the stack: the compose
infra, the app's /healthz + /readyz, pprof, the telemetry backends
(Prometheus, Tempo, Loki, Pyroscope) and Delve. These used to live in
` + "`forge doctor`" + `, which had to GUESS the app's port and reported the
miss as a gray dash indistinguishable from "not applicable". They run
here because this command already resolves the ports the stack actually
bound. A check that cannot determine its answer says UNDETERMINED — it
never reads as a pass.

Reads the same rendered KCL + resolved ports ` + "`forge env up`" + ` uses, so
the table matches what ` + "`forge env up <env>`" + ` printed — retrievable
after the startup scrollback is gone. ALL declared frontends are listed (a
project may declare several; each gets its own port row).

Whether the project itself is well-formed — deployability, payload caps,
tooling, cluster capability — is ` + "`forge doctor`" + `, which takes no env.

Examples:
  forge env status dev
  forge env status dev --signal traces   # one runtime signal only
  forge env status dev --json            # machine-readable for scripts/agents`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpServices(cmd.Context(), args[0], jsonOut, signal, verbose)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON (name/kind/url/port/log/listening/pid/owned + per-host-service serving[] build-freshness and a duplicate flag, plus the runtime checks)")
	cmd.Flags().StringVar(&signal, "signal", "", "Run only one runtime signal: app, metrics, traces, logs, profiles (default: all)")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Show evidence for all runtime checks (not just failures)")
	return cmd
}

// runUpServices renders the env's KCL, probes each declared port, and
// prints the host-service + frontend table (or JSON). It arms the SAME
// devstack render context `forge env up`/`forge env deploy` arm so ports resolve
// identically, then restores any resolve_port drift — this is a read-only
// report and must not shift the stable port assignments a live stack is on.
func runUpServices(ctx context.Context, env string, jsonOut bool, signal string, verbose bool) error {
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
	// doesn't list frontends `forge env up` never starts. Resolved against
	// THIS render (frontendPhaseEnabled), exactly as the up orchestrator
	// resolves it, so the report and the phase can never disagree. No
	// phase-skip log line here — that would pollute the --json output and is
	// meaningless (the `services` command runs no phase).
	frontendsOn := frontendPhaseEnabled(store, entities)
	// Collect the declared rows WITHOUT probing yet: the fresh render reverts
	// resolve_port drift, so a service on an ephemeral (bind :0) port renders
	// with no/wrong port here. Overlay the LIVE ports `forge env up` persisted
	// at launch, THEN probe — otherwise the probe (and the URL) target a port
	// the stack never bound.
	rows := collectUpServices(entities, env, nil, frontendsOn, nil)
	resolved := loadResolvedEnv(projectIDForDir(projectDir), env)
	if resolved != nil {
		overlayResolvedPorts(rows, resolved.Ports)
	}
	probeRowsListening(rows, portInUse)
	// Enrich with the listener pid + forge-ownership marker — the reclaim
	// guard's signal, reused here to distinguish "our stack is up" from "a
	// foreign process holds the port". Scoped to THIS project so another
	// project's stack on the same env name reads as unowned, not ours.
	enrichOwnership(rows, projectIDForDir(projectDir), env)
	// Enrich each host row with its live server process(es) + build-freshness,
	// and FLAG the air-leak symptom (more than one server per service). This is
	// the process-table half of the report: it sweeps for THIS project+env's
	// ownership marker (not just the port listener), so a duplicate/stale
	// worker that isn't the current port holder is still caught. headCommit is
	// the freshness yardstick (empty outside a git repo → no stale flags).
	headCommit := headCommitTime(projectDir)
	enrichServing(rows, projectIDForDir(projectDir), env, headCommit)

	// The env-runtime checks that used to live in `forge doctor`. Their
	// target is DERIVED from the rows above — one resolver for "what port
	// does this component serve on", and it is this one. See
	// env_status_checks.go.
	target := runtimeTargetFor(entities, rows)

	if jsonOut {
		checks, checkErr := runEnvRuntimeChecks(ctx, store.Meta().Name, projectDir, env, target, signal, true, verbose)
		if checkErr != nil {
			return checkErr // a mistyped --signal is a usage error, not a stack state
		}
		rep := upServicesReport{Env: env, Services: rows, Checks: checks.Checks}
		// DATABASE_URL is the other half of the discovery contract: an agent
		// or script gets this worktree's API port (per-service `port`) AND its
		// DSN from one call. Sourced from the launch-time persist; empty (and
		// omitted) when no live stack persisted it.
		if resolved != nil {
			rep.DatabaseURL = resolved.DatabaseURL
		}
		if !headCommit.IsZero() {
			rep.HeadCommitAt = headCommit.UTC().Format(time.RFC3339)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if len(rows) == 0 {
		fmt.Printf("[up] no host services or frontends declared in deploy/kcl/%s/\n", env)
	} else {
		// notReadyLabel "down": unlike the immediate post-launch summary (where a
		// not-yet-listening port means "still booting"), this snapshot runs long
		// after start, so a dead port is genuinely down.
		//
		// cluster=nil: `forge env status` prints the SAME verdict a few
		// lines below, as the "Cluster Workloads" runtime check with its own
		// -v evidence. Repeating it inside the box would be two renderings
		// of one fact that can drift apart.
		renderUpSummary(os.Stdout, env, rows, "down", true, nil, nil)
	}
	// Printed AFTER the table: the table is the answer most invocations
	// want, and the checks read as its detail. A project with no host rows
	// still gets them — its compose infra is runtime state too.
	_, err = runEnvRuntimeChecks(ctx, store.Meta().Name, projectDir, env, target, signal, false, verbose)
	return err
}

// newEnvDownCmd stops a running stack: this project's stack for one
// environment, or — with --all — every forge stack on the machine.
//
// --all is not a bigger hammer, it is the only reachable one for a stack whose
// project directory has been deleted: the per-project form derives the project
// id from the forge.yaml above the working directory, and there is none left to
// derive it from. Either form signals only processes carrying forge's own
// ownership markers.
func newEnvDownCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "down [environment]",
		Short: "Stop this project's stack for an environment (or --all: every forge stack on this machine)",
		Args:  cobra.MaximumNArgs(1),
		Long: `Stop a running ` + "`forge env up`" + ` / ` + "`forge run`" + ` stack.

  forge env down dev     stop THIS project's dev stack
  forge env down --all   stop every forge stack on this machine, all projects

Only processes forge itself started — the ones carrying its ownership
markers for the project and environment being stopped — are ever signalled.
A process forge did not start is never touched by either form.

Use --all when a stack outlived its project directory: without a forge.yaml
there is no project to scope to, and the per-environment form cannot reach
it. ` + "`forge env ps`" + ` lists what is running first.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				if len(args) > 0 {
					return fmt.Errorf("--all stops every environment; drop the %q argument", args[0])
				}
				return runUpStopAll()
			}
			if len(args) == 0 {
				return errors.New("name the environment to stop (e.g. `forge env down dev`), or pass --all for every forge stack on this machine")
			}
			return runUpStop(args[0])
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Stop every forge stack running on this machine, across all projects (the only way to reach a stack whose project directory is gone)")
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
// proxyPreflightTimeout bounds the proxy reachability probe. A local proxy
// answers a TCP connect in microseconds; anything past this is unusable for a
// dev loop either way.
var proxyPreflightTimeout = 2 * time.Second

// preflightProxyReachable fails fast when the environment names an HTTP proxy
// that nothing is listening on.
//
// A dead proxy is uniquely destructive because the environment routes
// EVERYTHING through it: the failure is not confined to the traffic the user
// wanted to inspect. It reaches kubectl talking to a cluster on this very
// machine, which is both the earliest and the least obvious casualty — the
// address k3d writes into kubeconfig is 0.0.0.0, and 0.0.0.0 matches no
// conventional NO_PROXY entry, so a loopback call gets proxied and dies.
//
// Only a REFUSED connection is treated as fatal. A proxy that accepts but is
// slow, or a name that does not resolve, is left to the individual call sites:
// the point here is to catch the unambiguous case where the proxy is simply
// not running.
func preflightProxyReachable(ctx context.Context, env []string) error {
	var raw, from string
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := envutil.Lookup(env, key); v != "" {
			raw, from = v, key
			break
		}
	}
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil // unparseable: not our call to reject
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}

	dialCtx, cancel := context.WithTimeout(ctx, proxyPreflightTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", host)
	if err == nil {
		_ = conn.Close()
		return nil
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return nil // slow/unresolvable — let the real calls report it
	}
	return fmt.Errorf(
		"%s=%s but nothing is listening on %s — every outbound call in this run would fail through it,\n"+
			"     including kubectl against your LOCAL cluster (k3d writes 0.0.0.0 into kubeconfig, which no NO_PROXY entry matches).\n"+
			"     start the proxy, or clear %s (and drop any -D flag that sets it) and re-run",
		from, raw, host, from)
}

func runUp(ctx context.Context, opts upOptions) error { //nolint:funlen // the `forge env up` lifecycle in order: resolve, render, conflict-check, launch, wait. The sequence is the contract.
	// PROXY PREFLIGHT, first thing. When the environment routes through an HTTP
	// proxy that is not actually listening, EVERY outbound call in the run
	// fails — kubectl against the local cluster, the package-manager install,
	// each host service's own traffic — and each one reports it differently
	// ("proxyconnect ... connection refused" buried under a kubectl retry
	// storm, an npm stall that looks like a hang). One check up front replaces
	// that cascade with the true cause, before any work is done.
	if err := preflightProxyReachable(ctx, os.Environ()); err != nil {
		return err
	}

	store, err := loadProjectStore()
	if err != nil {
		return err
	}
	cfg := store.Config()
	projectDir := projectDirForKCL()
	// Stable identity of THIS project, stamped onto every child and required to
	// match on every reclaim decision below. Keying ownership on (projectID,
	// env) — not env alone — is what stops the pre-flight reclaim / `env down`
	// here from reaping another project's stack that shares this env name.
	projectID := projectIDForDir(projectDir)

	// Arm the parallel-dev-stack render context: push the raw git facts
	// option("worktree")/option("branch") into KCL, back forge.allocate_port
	// with the lock-guarded block registry, and activate the resolve_port
	// store. `forge env deploy` arms the SAME inputs, so up and deploy resolve
	// identical ports — no drift. State is machine-local (.forge/blocks.json,
	// .forge/ports-*.json are gitignored). restorePortStore reverts the
	// resolve_port store if the already-running guard below rejects this
	// render (a rejected attempt must not drift the stable assignments).
	_, restorePortStore := activateDevStack(projectDir, opts.env)

	// Arm the -D render options BEFORE the first render. Validated against
	// what the env's KCL declares so a typo'd name fails here rather than
	// silently rendering the default (an option nothing reads is not an error
	// in KCL — it is simply unread). Validation degrades permissively if the
	// project's options can't be discovered; the render still runs.
	// Parse FIRST: it rejects malformed values and forge-reserved names, which
	// deserve their own message ("image_tag is set by forge") rather than the
	// declared-name check's "no such option" — image_tag is a real option, just
	// not yours.
	renderDArgs, err := parseRenderOptions(opts.renderOptions)
	if err != nil {
		return err
	}
	if err := validateRenderOptions(projectDir, opts.env, opts.renderOptions); err != nil {
		return err
	}
	setRenderOptions(renderDArgs)

	fmt.Printf("[up] env=%s\n", opts.env)
	entities, err := RenderKCL(ctx, projectDir, opts.env)
	if err != nil {
		return fmt.Errorf("render KCL: %w", err)
	}
	// Frontends are a forge.yaml concept the deploy-as-data env templates
	// don't project into the KCL `output` contract, so the render carries
	// none. Bridge cfg.Frontends into the entity set BEFORE the empty-check
	// and summary so a frontend-only project isn't misread as "nothing
	// declared" and the frontend phase actually has servers to launch.
	mergeConfigFrontends(entities, cfg)
	// Materialize any cross-repo frontend source BEFORE the frontend phase
	// reads a Path — `upFrontends` uses it as the dev server's cmd.Dir, and a
	// `source:` frontend has no `path` in the render. Left unresolved, cmd.Dir
	// is empty, so npm inherits forge's own working directory and dies on the
	// project root's missing package.json ("Could not read package.json"),
	// which reads as a dependency problem rather than a wiring one.
	//
	// build and deploy already resolve through this same resolver; up was the
	// one lane outside that invariant, so a GitSource frontend could be built
	// and deployed but never dev-served. No-op for a project that declares no
	// sources.
	if err := resolveFrontendEntitySources(ctx, projectDir, entities); err != nil {
		return fmt.Errorf("resolve frontend sources: %w", err)
	}
	if entitiesEmpty(entities) {
		return fmt.Errorf("no services/operators/frontends/cronjobs declared in deploy/kcl/%s/", opts.env)
	}

	// A --target that names nothing is a typo, and the cost of treating it
	// as a filter that simply matches no entity is the worst outcome
	// available: the pre-flight below still tears down the running stack,
	// and then the run starts nothing in its place. Validated AFTER
	// mergeConfigFrontends so frontend names are in the available set, and
	// against the SAME name set `forge env deploy --target` validates
	// against, so a name that works for one command works for the other.
	if err := validateDeployTargets(entities, opts.targets); err != nil {
		return err
	}
	planEntities := entities
	if len(opts.targets) > 0 {
		planEntities = filterEntitiesByTarget(entities, opts.targets)
	}
	summarizeKCLBuildPlan(planEntities)

	// `forge env up` always runs the whole dev loop — cluster build+deploy,
	// host, frontend — with --target narrowing WHICH entities inside each
	// phase, never which phases run. A service that should run as a host
	// process during dev says so declaratively in its KCL
	// (`host = forge.HostOverrides {...}`, e.g. `-D host_runner=go-run` to
	// switch the launch runner); forge never rewrites a cluster-declared
	// entity's deploy type based on a flag.

	// One-shot jobs are the exception that proves that rule, and it is an
	// argv translation rather than a placement decision: runHostJobs execs
	// them HERE, on this machine, but their `command` is written for the
	// image (`/app/<project> db migrate up`) — a path that does not exist on
	// a developer's laptop. Rewriting argv[0] to the go-run target does not
	// move a workload anywhere; it just spells the same binary the way the
	// host can reach it.
	collapseJobsToHost(entities, cfg.Name)

	// apiBaseURL is the ephemeral backend base URL wired into the frontends
	// (via NEXT_PUBLIC_API_URL / VITE_API_URL / EXPO_PUBLIC_API_URL) so they
	// reach the backend on its allocated port instead of the baked default.
	// Only host services that declare no port of their own get one — see
	// resolveEphemeralHostPorts.
	apiBaseURL := resolveEphemeralHostPorts(entities)
	resolveEphemeralFrontendPorts(cfg, entities)

	// Build the per-env secret provider ONCE for this run (dotenv reads
	// the file now; external/none are cheap no-ops). Reused for both the
	// fail-fast validation below and the host-service env injection in
	// upHostServices — building it here avoids re-reading the dotenv per
	// service. The host phase always runs, so validate up front that every
	// host service's declared secret_ref resolves against the provider
	// before any process starts. ValidateDeclaredRefs is a no-op for
	// external/none providers, so this only bites a dotenv provider missing
	// a declared key (and lists every miss at once).
	prov, err := secretProviderFromEntities(entities, projectDir)
	if err != nil {
		return fmt.Errorf("secret provider: %w", err)
	}
	dotenvPath := ""
	if entities.SecretProvider != nil {
		dotenvPath = entities.SecretProvider.Path
	}
	if err := secrets.ValidateDeclaredRefs(prov, secretRefsForHostServices(entities), dotenvPath); err != nil {
		return err // already actionable; lists every missing key
	}

	// Cluster phases — build + deploy. Both are feature-gated: if the
	// project's forge.yaml turns either off (`features.build: false`
	// or `features.deploy: false`), the orchestrator skips the phase
	// with a one-line log and continues. Direct `forge build` /
	// `forge env deploy` invocations still error — see requireFeature
	// in feature_gate.go for the strict-gate shape used by the cobra
	// RunE for those commands.
	if err := upBuildDeployPhases(ctx, upClusterInput{
		store: store, cfg: cfg, entities: entities, projectDir: projectDir,
		opts: opts,
	}); err != nil {
		return err
	}

	// Whether this run asserts on Kubernetes workload health at the end.
	// Resolved once, from the feature/target facts already in hand, so
	// both terminal paths below gate identically.
	clusterGate := upClusterGateEnabled(store, entities, opts.targets)

	// When --no-build skipped the build phase, the host runners (air /
	// go-run) still compile against gen/, so ensure generated code is
	// present here too — otherwise host services fail with the same
	// "cannot load module gen" error the build phase would have
	// pre-empted. No-op when up-to-date or --no-generate. (The non-skipped
	// path already ran this inside runBuild.)
	if opts.noBuild {
		if err := ensureGeneratedCode(projectDirForKCL(), opts.noGenerate); err != nil {
			return fmt.Errorf("ensure generated code: %w", err)
		}
	}

	// Pre-flight. Respects --target (only the services this invocation would
	// start are considered) and the frontend feature gate.
	frontendsOn := frontendPhaseEnabled(store, entities)
	if err := upPreflight(projectID, opts.env, entities, opts.targets, frontendsOn); err != nil {
		// Undo any resolve_port drift this rejected render persisted, so the
		// next clean run still gets the canonical port assignments.
		restorePortStore()
		return err
	}

	// Resolve the lifecycle (Part B): supervise (hold + Ctrl-C teardown)
	// vs once (start, persist PIDs, return). `up`'s default is auto —
	// resolved here by the TTY check so an agent / CI `forge env up` doesn't
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
	// all down together. The registry writes its ledger + this project's path
	// record on every start, so a child is on disk — and therefore reachable by
	// `forge env down` / `forge env ps` — from the moment it exists, however
	// this process returns.
	procs := newProcRegistry(projectID, projectDir, opts.env)

	// Phase 3: host-mode services.
	if err := upHostPhase(ctx, hostPhase{
		cfg: cfg, entities: entities, prov: prov, opts: opts,
		projectDir: projectDir, detach: detach, procs: procs,
	}); err != nil {
		return err
	}

	// Phase 4: frontends. Skipped when the project EXPLICITLY sets
	// features.frontend: false — the orchestrator otherwise tries to
	// npm-run-dev a tree that the project never scaffolded. The skip line
	// is printed only when the render actually declares frontends: on a
	// backend-only project there is nothing to elide and the line was
	// pure noise.
	if frontendPhaseEnabled(store, entities) {
		feFailures := upFrontends(ctx, frontendLaunch{
			entities: entities, env: opts.env, background: detach,
			noInstall: opts.noInstall, targets: opts.targets,
			frontendArgs: opts.frontendArgs, apiBaseURL: apiBaseURL, procs: procs,
		})
		if feFailures > 0 {
			fmt.Printf("[up] %d frontend(s) failed to start (see above)\n", feFailures)
		}
	} else if len(entities.Frontends) > 0 {
		fmt.Printf("[up:frontend] feature 'frontend' is disabled in forge.yaml — skipping %d frontend(s)\n",
			len(entities.Frontends))
	}

	// Persist the RESOLVED discovery facts (name→port map + DATABASE_URL)
	// alongside the PID ledger, BEFORE the readiness gate can return an error.
	// Ephemeral (bind :0) ports are assigned in THIS process and would otherwise
	// vanish once the launch scrollback scrolls off: `forge env status`
	// re-renders the KCL from scratch (and reverts resolve_port drift), so it
	// can't recover the live port. A failed readiness gate is exactly when
	// someone needs to know which port the thing that didn't come up was on.
	persistResolvedEnv(projectID, opts.env, entities, cfg, opts.targets, frontendsOn)

	// Post-launch readiness gate. The host phase only confirmed the runners
	// FORKED — not that their children actually bound their ports. Before we
	// paint a (potentially misleading) green summary, wait until OUR OWN
	// marked child is the listener on every expected host-service port, or
	// fail loudly. This catches both a silent bind failure (nothing ever
	// listens) and the stale-holder trap (a foreign/old process still answers
	// the probe) — either of which the best-effort summary used to show as
	// "up". Detect and report only: nothing is killed here, and the children
	// this run started are already tracked, so the next `forge env up` / `forge
	// run` for this project+env reclaims them and `forge env down` stops them.
	// Scoped by --target inside the gate.
	if err := waitHostServicesReady(entities, projectID, opts.env, opts.targets, hostReadyTimeout, hostReadyPoll); err != nil {
		return err
	}
	// First-boot dev auto-seed. By this point the app's AUTO_MIGRATE has
	// run (readiness gate passed), so the schema is current. Warn-never-
	// fatal: a seed failure never breaks the dev loop.
	maybeAutoSeed(ctx, store, cfg, entities, opts)

	// The cluster half of the readiness gate, taken as late as possible: the
	// deploy phase already waited each rollout out, and the host phase since
	// then is free settling time. One look, not a poll — see
	// evalClusterWorkloads.
	clusterHealth := evalClusterWorkloads(ctx, clusterGate, projectDir, opts.env)

	// Summary box: what's listening where, and where to find each
	// service's log. Printed in both the supervise and detach paths so the
	// URLs + log paths are one glance away (and greppable for an agent).
	// frontendsOn (computed above for the port-conflict guard) is reused so
	// the summary lists exactly the frontends this run actually started.
	// The cluster verdict rides in the same box: the half of "what came up"
	// that runs in Kubernetes was missing from it entirely.
	printUpSummary(entities, opts.env, detach, opts.targets, frontendsOn, clusterHealth)

	clusterErr := clusterWorkloadError(opts.env, clusterHealth)

	if detach {
		fmt.Printf("[up] detached %d process(es). Stop with `forge env down %s`.\n",
			procs.count(), opts.env)
		return clusterErr
	}

	if clusterErr != nil {
		// Do NOT hold the foreground on an env that is not up: the hold
		// exits 0 on Ctrl-C, which is the green-exit-over-a-crashloop this
		// gate exists to end. Nothing is killed — the host children are in
		// the ledger, so `forge env down` still reaches them.
		fmt.Printf("[up] host/frontend process(es) from this run are still running — stop them with `forge env down %s`.\n", opts.env)
		return clusterErr
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

// hostPhase is the state upHostPhase needs to converge the host world and
// start the host-mode services in it.
type hostPhase struct {
	cfg        *config.ProjectConfig
	entities   *KCLEntities
	prov       secrets.Provider
	opts       upOptions
	projectDir string
	detach     bool
	procs      *procRegistry
}

// upHostPhase runs phase 3 of `forge env up`: converge the host world, then
// start the host-mode services in it.
//
// The ordering is the load-bearing part. Infrastructure and the dev database
// come up first, then the one-shot jobs run to completion, then the config
// those jobs published is re-read, and only then do services start — each step
// prepares the world the next one assumes.
func upHostPhase(ctx context.Context, p hostPhase) error {
	cfg, entities, opts := p.cfg, p.entities, p.opts
	{
		// Converge the env's DECLARED infrastructure before any host process
		// dials it. The cluster phase already kicks off the same pre-warm
		// concurrently with the build (see upBuildDeployPhases) — but that
		// one is skipped under --no-deploy,
		// and prewarmInfra/hostinfra.Start are idempotent (a running
		// instance is adopted, not restarted), so calling it again here
		// unconditionally is what guarantees a host service's dependencies
		// are up regardless of which build/deploy flags this run passed.
		// Without it a run that skips the cluster phase's pre-warm (or ran
		// before this project declared any cluster workloads at all) starts
		// an app against whatever happened to be listening on the
		// scaffolded DSN's port — including another project's postgres,
		// silently, with the right port and the wrong database.
		//
		// This is deliberately generic: it brings up whatever the env's KCL
		// declares as infrastructure and knows nothing about what those
		// servers ARE. The scaffolded dev env declares postgres as a
		// `forge.HostInfra` — a real postgres forge runs as a HOST PROCESS,
		// so a `forge run` needs no container runtime at all — and (only when
		// the project ships a frontend) the dev IdP as a `forge.Compose`
		// container. A project that wants its database containerized too says
		// `forge.Compose` there as well and this loop brings that up instead.
		//
		// Best-effort by the same reasoning as the cluster path's pre-warm: a
		// project may legitimately run its infra out of band, and the
		// readiness gate below is the authoritative check on whether the
		// stack actually came up.
		if err := prewarmInfra(ctx, opts.env, entities); err != nil {
			fmt.Printf("[up] infra: %v (continuing; host services may fail to connect)\n", err)
		}
		// Ensure the dev database the host services are about to dial EXISTS
		// before they boot — the runtime counterpart to the generate-time
		// shadow DB, which forge already ensure-creates on the fly. A freshly
		// scaffolded dev DSN (…:5434/<project>) names a database nothing has
		// issued CREATE DATABASE for, so the first `forge run` boot would
		// otherwise die with `FATAL: database "<project>" does not exist`
		// before AUTO_MIGRATE could apply the schema. ensureDevDatabase is a
		// no-op off dev (seedTargetIsDev gates it) and off a resolved DSN, so
		// running it unconditionally here costs nothing on staging/prod.
		if err := ensureDevDatabase(cfg, entities, opts.env); err != nil {
			return err
		}
		// Identity needs no SPECIAL step here — the idp-provision job below
		// IS the convergence step, run through the exact same runHostJobs
		// path as any other one-shot. The values it publishes (the
		// generated client_id, the project id) reach both halves of the
		// app as ordinary per-env config, by importing the committed file
		// it writes, like every other declared value — there is no
		// separate bring-up step to skip, reorder, or forget.
		//
		// One-shot jobs run to completion BEFORE any host service starts —
		// that is the ordering the `job` component kind declares, and on
		// the host there is no orchestrator underneath us to enforce it.
		// Fail-closed: a job that does not exit 0 stops the up rather than
		// letting its dependents run against a world it was supposed to
		// have prepared.
		if err := runHostJobs(ctx, cfg, entities, p.prov.All(), opts.env); err != nil {
			return err
		}
		// Re-read what those jobs PUBLISHED into the environment's config.
		//
		// idp-provision registers the browser application and writes the
		// generated client_id into deploy/kcl/<env>/idp_identity_gen.k. The
		// frontend's config.js was rendered by `forge generate`, BEFORE that
		// file had a real value in it — so on a fresh project the browser
		// would be served an empty OIDC_CLIENT_ID (the frontend's no-auth
		// posture) until someone ran generate a second time, with nothing on
		// screen saying so. The value cannot exist any earlier: it is minted
		// by a running IdP on request. So it is re-read here, once the jobs
		// that produce it have run and before the frontend serves anything.
		//
		// A no-op from the second run on, and never fatal — see
		// refreshFrontendRuntimeConfigs.
		if changed, err := refreshFrontendRuntimeConfigs(cfg, p.projectDir, opts.env); err != nil {
			fmt.Printf("[up] frontend runtime config: %v (serving the previously generated config.js)\n", err)
		} else if changed > 0 {
			fmt.Printf("[up] frontend runtime config: refreshed %d config.js from converged identity\n", changed)
		}
		hostFailures := upHostServices(ctx, cfg, entities, p.prov, opts.env, p.detach, opts.targets, p.procs)
		if hostFailures > 0 {
			fmt.Printf("[up] %d host service(s) failed to start (see above)\n", hostFailures)
		}
	}
	return nil
}

// upPreflight is everything that must hold before this run starts a process.
//
//  1. ONE stack per (project, env). A predecessor — detached, orphaned by a
//     crash, whatever — is STOPPED, never adopted: this invocation may carry a
//     different config, a different kernel-assigned port, or reinstalled deps,
//     so the running process is not the process the caller asked for. The
//     decision reads the live ownership markers and is entirely
//     PORT-INDEPENDENT, which is the whole point. It used to hang off a port
//     conflict, and then ephemeral dev ports arrived: a second stack takes a
//     free port, collides with nothing, and the reclaim never ran. Eight rounds
//     of `forge run` left 38 orphaned processes, 7.5 GB resident, on 15 ports.
//
//  2. Refuse to start against a port held by something we do NOT own. That is
//     all the port probe was ever for. After (1) every remaining holder is
//     foreign by construction — a foreign process is reported, never killed.
func upPreflight(projectID, env string, e *KCLEntities, targets []string, frontendsOn bool) error {
	stopped := stopStackScoped(projectID, env, targets)
	if stopped > 0 {
		scopeNote := ""
		if len(targets) > 0 {
			scopeNote = fmt.Sprintf(" for %s (other services left running)", strings.Join(targets, ", "))
		}
		fmt.Printf("[up] stopped %d process tree(s) from the env=%s stack this project was already running%s\n", stopped, env, scopeNote)
	}
	conflicts := conflictingPorts(e, targets, frontendsOn, portInUse)
	if len(conflicts) == 0 {
		return nil
	}
	// A teardown does not free ports synchronously, so a port still bound
	// THIS instant is not yet evidence of anything.
	//
	// killTreesAndWait waits only on the tree ROOTS it signalled. The process
	// actually holding the port is usually a DESCENDANT — `go run` execs the
	// binary it compiled, air re-execs the server — which is never in that
	// list and so is never waited on. The final SIGKILL pass is not waited on
	// either. Probing here therefore raced a listener that was already on its
	// way out, and reported a stack forge had just successfully stopped as one
	// that "still holds these ports", telling the user to run `forge env down`
	// against processes that no longer existed by the time they read it.
	//
	// So: re-probe the conflicting ports over a bounded grace, and treat only
	// what SURVIVES it as a conflict. Gated on stopped > 0 because nothing was
	// released if nothing was killed — a purely foreign holder still fails
	// fast, with no waiting.
	if stopped > 0 {
		conflicts = awaitPortRelease(conflicts, portInUse, portReleaseGrace, portReleasePollInterval, time.Sleep)
		if len(conflicts) == 0 {
			return nil
		}
	}
	// Classified against a FRESH process snapshot — the teardown above changed
	// the table, so a snapshot taken before it would be stale. Anything still
	// holding a port is foreign; an `owned` entry here means a tree-kill that
	// waits for exit did not take, which is a different failure and says so.
	owned, foreign := classifyPortConflicts(projectID, env, conflicts, portListenerPID, newOSProcFacts())
	return portHolderError(env, owned, foreign)
}

// portHolderError renders the refusal when ports this run needs are still
// held. The two arms are different failures with different remedies: a marked
// holder survived a teardown that should have taken it (report the pid, do not
// retry blindly), and a holder forge never started, which forge will not kill.
func portHolderError(env string, owned, foreign []portConflict) error {
	var b strings.Builder
	if len(owned) > 0 {
		_, _ = fmt.Fprintf(&b, "[up] a forge-managed stack for env=%s still holds these ports after being stopped:\n", env)
		for _, c := range owned {
			_, _ = fmt.Fprintf(&b, "       %-14s :%d\n", c.name, c.port)
		}
		_, _ = fmt.Fprintf(&b, "     inspect:  forge env ps        (what forge is running, machine-wide)\n")
		_, _ = fmt.Fprintf(&b, "     stop:     forge env down %s", env)
	}
	if len(foreign) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		_, _ = fmt.Fprintf(&b, "[up] these ports are held by a process forge doesn't manage (env=%s):\n", env)
		for _, c := range foreign {
			_, _ = fmt.Fprintf(&b, "       %-14s :%d\n", c.name, c.port)
		}
		b.WriteString("     free them (lsof -i :<port>, kill the PID) or use --target to start a different service")
	}
	return errors.New(b.String())
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
}

// upBuildDeployPhases runs the build/infra/deploy side of `forge env up`.
// Explicit targets first reduce that work to the selected entity graph: a
// host service, build-only service, or dev-served frontend has no cluster
// deployment edge, so cluster creation, kubeconfig minting, and deploy are
// skipped entirely. Compose/external targets still deploy without touching a
// cluster; cluster services, operators, and platform charts require both.
//
// Infra pre-warm remains independent from cluster deployment. Host processes
// can depend on the env's compose/host-infra services even when none of the
// selected applications runs in Kubernetes.
func upBuildDeployPhases(ctx context.Context, in upClusterInput) error {
	store, cfg, entities, opts := in.store, in.cfg, in.entities, in.opts
	required := targetPhaseRequirements(entities, opts.targets)
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
	if required.cluster && !opts.noDeploy && !skipFeature(store, config.FeatureDeploy, "up:clusters") {
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
	// Kick off the declared infra (host-run postgres, and any compose
	// service the env declares) NOW, concurrent with the build phase.
	// Bringing a server up — an image pull and health warmup for a
	// container, a first-run binary extraction for a host process — is the
	// long pole on a warm `up` and is wholly independent of the project
	// image build, so overlapping the two shaves that wall-clock off every
	// run. The deploy phase below re-dispatches the same groups as an
	// idempotent no-op once warm (pull is current, `up -d` sees the
	// containers running, and a host instance already serving is adopted)
	// and stays the authoritative health barrier before the k8s rollout —
	// so a best-effort failure here is non-fatal. Skipped when the deploy
	// phase won't run (--no-deploy): nothing would consume the infra and
	// nothing later barriers on it.
	// FRONTEND DEPENDENCY PREFLIGHT. The install used to run in the frontend
	// phase, which is LAST — so a frontend whose deps cannot be installed
	// failed the run only after the image build had already spent minutes,
	// and every retry paid that cost again before reaching the same failure.
	// The check is free in the steady state (frontendDepsStale short-circuits
	// when node_modules is current), so hoisting it costs nothing on a warm
	// run and converts the cold failure from "5 minutes, then broken" into
	// "broken now". upFrontends still calls it and is then a no-op.
	if frontendPhaseEnabled(store, entities) && !opts.noInstall {
		if err := preflightFrontendDeps(ctx, entities, opts.targets); err != nil {
			return err
		}
	}

	var infraWarm chan error
	if !opts.noDeploy && !skipFeature(store, config.FeatureDeploy, "up:infra") {
		infraWarm = make(chan error, 1)
		go func() {
			fmt.Println("\n[up] infra phase (concurrent with build)")
			infraWarm <- prewarmInfra(ctx, opts.env, entities)
		}()
	}
	if !opts.noBuild {
		if !skipFeature(store, config.FeatureBuild, "up:build") {
			fmt.Println("\n[up] build phase")
			if err := upBuildCluster(ctx, cfg, opts.env, opts.noGenerate, opts.targets); err != nil {
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
	if required.deploy && !opts.noDeploy {
		if !skipFeature(store, config.FeatureDeploy, "up:deploy") {
			fmt.Println("\n[up] deploy phase")
			// Cluster reconcile through the SAME named entry point
			// `forge env deploy` uses. `up`'s cluster step carries a
			// scope-derived deployOptions — instead of a blank
			// `deployOptions{}` literal standing in for "deploy with no
			// options."
			//
			// skipFrontend: true is the deploy-phase mirror of the build
			// phase's skipFrontends (upBuildCluster). `forge env up` ALWAYS
			// dev-serves frontends in its Phase 4 (`npm run dev` =
			// `next dev` / vite dev server) and NEVER consumes a prod
			// frontend artifact, so the deploy phase must not run the
			// `deploy = None` build-only path (dispatchFrontendDeploys →
			// `npm run build` under NODE_ENV=production). Without this a
			// bare `forge env up dev` would prod-`next build` every
			// host-mode frontend (the static `output: "export"` Next.js
			// build) right before — and pointlessly alongside — starting
			// its `next dev` server. The build-only path exists to
			// materialize a static frontend for a FirebaseHosting frontend
			// to reference at DEPLOY time; it has no place in the dev loop.
			if err := reconcileCluster(ctx, opts.env, deployOptions{skipFrontend: true, targets: opts.targets}); err != nil {
				return fmt.Errorf("deploy: %w", err)
			}
		}
	}
	return nil
}

// upPhaseRequirements is the deployment-side closure of an explicit target
// set. Build/host/frontend selection is already name-filtered by their own
// phases; these flags answer the two expensive questions that cannot be
// inferred from a non-empty --target alone: does anything selected need a
// deploy provider, and does any selected provider require Kubernetes?
type upPhaseRequirements struct {
	deploy  bool
	cluster bool
}

// targetPhaseRequirements derives phase requirements from the rendered
// placement graph. With no explicit targets, preserve the full declarative
// reconcile. With targets, only selected entities contribute requirements.
//
// Frontends are dev-served by `env up`, so they do not invoke their production
// deploy provider here. A cluster frontend is the exception: it also renders a
// target-labelled Kubernetes workload, which the existing targeted apply path
// must continue to reconcile.
func targetPhaseRequirements(e *KCLEntities, targets []string) upPhaseRequirements {
	if len(targets) == 0 {
		return upPhaseRequirements{deploy: true, cluster: true}
	}

	var out upPhaseRequirements
	for _, svc := range e.Services {
		if !inTargetSet(targets, svc.Name) {
			continue
		}
		switch svc.Deploy.Type {
		case "cluster":
			out.deploy = true
			out.cluster = true
		case "compose", "external", "host-infra":
			out.deploy = true
		}
	}
	for _, op := range e.Operators {
		if inTargetSet(targets, op.Name) {
			out.deploy = true
			out.cluster = true
		}
	}
	for _, chart := range e.HelmCharts {
		if inTargetSet(targets, chart.Name) {
			out.deploy = true
			out.cluster = true
		}
	}
	for _, frontend := range e.Frontends {
		if inTargetSet(targets, frontend.Name) && frontend.Deploy != nil && frontend.Deploy.Type == "cluster" {
			out.deploy = true
			out.cluster = true
		}
	}
	return out
}

// upServiceRow is one host service / frontend line shared by the immediate
// `forge env up` summary and the retrieve-after-the-fact `forge env status`
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
	// Serving lists the live server process(es) actually running this host
	// service's binary — the LEAF process under its runner (air / go-run),
	// discovered by sweeping the process table for this project+env's
	// ownership marker rather than by the port listener alone (a service may
	// not listen, and a stale straggler may have lost the port to a rebuild).
	// Each carries build-freshness: binary path, its mtime, the process start
	// time, and a stale flag. Normally exactly one; EMPTY for a frontend, a
	// down/foreign-held service, or a platform without process inspection.
	// Filled only by the `forge env status` path (enrichServing).
	Serving []servingProc `json:"serving,omitempty"`
	// Duplicate is the air-leak alarm: more than one server process is running
	// THE SAME COMMAND under this row (a rebuild that did not reap its
	// predecessor, or an orphaned straggler) — len(Duplicates) > 0. Surfaced
	// as its own bool so a dashboard/agent alerts on it without counting.
	Duplicate bool `json:"duplicate,omitempty"`
	// Duplicates names WHICH command is duplicated, one entry per command with
	// more than one live process. The marker a process is grouped by rides the
	// environment and so propagates to descendants, which means a row's
	// processes are not necessarily all the same program; attribution comes
	// from argv (see attributeDuplicates), so a duplicated `reliant server
	// worker` is reported as the worker rather than as whichever row it
	// happened to inherit its marker from.
	Duplicates []duplicateGroup `json:"duplicates,omitempty"`
	// AttributionUndetermined is set when this row has several serving
	// processes and at least one has NO readable argv (a platform with no argv
	// source, or a process whose cmdline the kernel withholds). The duplicate
	// question cannot be answered honestly for it, so the report says so
	// instead of guessing — a confident wrong attribution is the defect this
	// whole path exists to avoid.
	AttributionUndetermined bool `json:"attribution_undetermined,omitempty"`
}

// duplicateGroup is one duplicated command under a service row: the argv-derived
// command identity and every live pid running it. Emitted under
// upServiceRow.Duplicates; additive JSON contract.
type duplicateGroup struct {
	// Command is the argv-derived identity (`reliant server worker`) — what is
	// actually duplicated, which is not necessarily the row's name.
	Command string `json:"command"`
	// PIDs are the live processes running Command, sorted. Always len > 1.
	PIDs []int `json:"pids"`
}

// servingProc is one live server process backing a host service, with the
// build-freshness facts that answer "is this the fresh build?" without ps/stat
// archaeology. Emitted under upServiceRow.Serving; additive JSON contract.
type servingProc struct {
	PID int `json:"pid"`
	// Path is the executable the process is running (air's built `tmp/main`,
	// a go-run temp binary, …). On Linux a binary replaced under a running
	// process shows a trailing " (deleted)" — a straggler tell that is kept,
	// not stripped.
	Path string `json:"path,omitempty"`
	// BuiltAt is the executable's on-disk mtime (RFC3339 UTC); empty when the
	// path is unstatable or has been deleted out from under the process.
	BuiltAt string `json:"built_at,omitempty"`
	// StartedAt is when the process started (RFC3339 UTC). A start that
	// predates BuiltAt means the process is running an older build than the
	// one now on disk — the straggler signal behind a duplicate.
	StartedAt string `json:"started_at,omitempty"`
	// Argv is the process's command line as the kernel reports it. Empty when
	// argv is unreadable, which is what makes duplicate attribution report
	// UNDETERMINED rather than guess.
	Argv []string `json:"argv,omitempty"`
	// Command is the argv-derived command identity duplicate attribution groups
	// by (`reliant server worker`) — see processCommand. Empty exactly when
	// Argv is.
	Command string `json:"command,omitempty"`
	// Stale is true when this process is NOT running current code: its binary
	// predates the repo HEAD commit, OR the on-disk binary was rebuilt after
	// the process started, OR the binary was deleted under it.
	Stale bool `json:"stale,omitempty"`
}

// upServicesReport is the `forge env status --json` envelope. Stable so
// scripts / sub-agents can consume it. Fields are additive: a new key never
// changes the meaning of an existing one, so a consumer that pins to the old
// shape keeps working.
type upServicesReport struct {
	Env string `json:"env"`
	// DatabaseURL is the DSN the live stack's services dial, captured by
	// `forge env up` at launch (resolveSeedDSN) and persisted alongside the
	// PID ledger. Empty/omitted when no stack has been brought up for this env
	// in this worktree, or the launch predated this field. One call therefore
	// yields both this worktree's API port (per-service `port`) and its DSN.
	DatabaseURL string `json:"database_url,omitempty"`
	// HeadCommitAt is the repo HEAD commit time (RFC3339 UTC) that per-service
	// build-freshness is measured against: a serving process whose binary
	// predates it is flagged stale. Empty when the project dir is not a git
	// repo / git is unavailable. Omitted so a consumer pinned to the old shape
	// is unaffected.
	HeadCommitAt string         `json:"head_commit_at,omitempty"`
	Services     []upServiceRow `json:"services"`
	// Checks are the env-runtime health checks (compose infra, app
	// /healthz, pprof, telemetry backends, Delve) — the set that moved off
	// `forge doctor` when doctor stopped answering runtime questions.
	// Additive: a consumer reading only `services` is unaffected. A check
	// whose status is "unknown" is UNDETERMINED, not a pass.
	Checks []doctor.CheckResult `json:"checks,omitempty"`
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
func enrichOwnership(rows []upServiceRow, projectID, env string) {
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
		if _, ok := forgeOwnerOfPID(pid, projectID, env, facts); ok {
			rows[i].Owned = true
		}
	}
}

// printUpSummary prints a compact box of what `forge env up` just brought
// up: each host service and frontend, its URL (when a listen port is
// known), a point-in-time listen check, and the path to its log file —
// plus where to grep all logs, how to list cluster routes, and how to
// re-list this table later. Mirrors the cloud-dev "final banner" so a
// developer (or an LLM agent) can find URLs and logs at a glance instead
// of scraping them out of interleaved startup scrollback.
//
// Health here is best-effort: the processes JUST started, so a service
// still binding its port shows "starting" (not "down"). Use
// `forge env status <env>` for the settled status.
// cluster is this run's rolled-up Kubernetes workload verdict, or nil when
// the run has nothing to say about a cluster (see clusterWorkloadSummary).
// It is what makes the box tell the truth about BOTH halves of what was
// brought up; a box that lists only host processes is the same silence the
// exit code used to keep.
func printUpSummary(e *KCLEntities, env string, background bool, targets []string, frontendsOn bool, cluster *clusterWorkloadSummary) {
	rows := collectUpServices(e, env, targets, frontendsOn, portInUse)
	if len(rows) == 0 && cluster == nil {
		return
	}
	var trailers []string
	// The lifecycle trailer is about the host/frontend children. With no
	// rows there are none, and "Ctrl-C to stop." / "stop with forge env
	// down" would be advice about processes that do not exist.
	if len(rows) > 0 {
		trailer := "Ctrl-C to stop."
		if background {
			trailer = fmt.Sprintf("Detached — stop with `forge env down %s`", env)
		}
		trailers = append(trailers, trailer)
	}
	// A stack whose two identity halves disagree comes up looking perfectly
	// healthy — every process green, every port bound — and then answers 401
	// to every authenticated RPC. The banner is the one place someone is
	// definitely looking at that moment, so the warning belongs here rather
	// than only in a `forge doctor` run nothing prompts them to make.
	if w := authParityTrailer(env); w != "" {
		trailers = append(trailers, w)
	}
	// showOwner=false: the immediate summary stays off the lsof/ps hot path;
	// notReadyLabel="starting" because a just-launched port not yet bound is
	// booting, not down.
	renderUpSummary(os.Stdout, env, rows, "starting", false, trailers, cluster)
}

// authParityTrailer returns a one-line banner warning when this environment's
// backend and frontend do not agree on an issuer, or "" when they do (or when
// neither declares one, which is the correct closed-and-bootable state).
//
// It reuses doctor's check rather than re-reading the KCL, so there is one
// definition of "the halves disagree" and the banner cannot drift from what
// `forge doctor` reports. Warn-only for the same reason doctor warns: a
// split-issuer setup is legitimate, and this cannot tell one from a typo.
func authParityTrailer(env string) string {
	root, err := os.Getwd()
	if err != nil {
		return ""
	}
	return authParityTrailerIn(root, env)
}

// authParityTrailerIn is authParityTrailer against an explicit project root.
func authParityTrailerIn(root, env string) string {
	res := doctor.CheckAuthParity(context.Background(), &doctor.Environment{ProjectDir: root, Env: env})
	if res.Status != doctor.StatusWarn {
		return ""
	}
	return "⚠ Auth: " + res.Message + "\n" +
		"│   Sign-in will succeed and RPCs will still 401. Details: forge doctor"
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
//   - cluster is the env's Kubernetes workload verdict, printed as its own
//     group; nil omits the group entirely. A box with no rows but a cluster
//     verdict still prints — that is a run over a purely in-cluster env,
//     whose whole output IS the cluster.
func renderUpSummary(w io.Writer, env string, rows []upServiceRow, notReadyLabel string, showOwner bool, trailers []string, cluster *clusterWorkloadSummary) {
	if len(rows) == 0 && cluster == nil {
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
		// Duplicate first — the loud line — then each serving process's
		// build-freshness so the vintages are visible side by side. Both are
		// only ever populated on the `forge env status` path (enrichServing).
		// The command named here is the argv-derived one that is ACTUALLY
		// duplicated, which is not necessarily this row's name: the ownership
		// marker propagates to descendants, so a row can carry a process
		// running a different program.
		for _, d := range r.Duplicates {
			fmt.Fprintf(w, "%s       ⚠ DUPLICATE: %d processes running %q (pids %s) — a rebuild likely didn't reap the old one; kill the stale pid\n",
				bar, len(d.PIDs), d.Command, joinInts(d.PIDs))
		}
		if r.AttributionUndetermined {
			fmt.Fprintf(w, "%s       ⚠ %d processes under this service, but argv was unreadable for at least one — cannot determine which command is duplicated\n",
				bar, len(r.Serving))
		}
		for _, sp := range r.Serving {
			fmt.Fprintf(w, "%s          %s\n", bar, servingLine(sp))
		}
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
	fmt.Fprintf(w, "╭─ forge env up · %s ─────────────────────────────────────\n", env)
	if anyDuplicateServing(rows) {
		fmt.Fprintf(w, "%s ⚠ DUPLICATE SERVER PROCESSES DETECTED — see the flagged service(s) below\n", bar)
	}
	if anyAttributionUndetermined(rows) {
		fmt.Fprintf(w, "%s ⚠ SOME PROCESSES COULD NOT BE ATTRIBUTED (argv unreadable) — see the flagged service(s) below\n", bar)
	}
	// The loud line, in the same register as the duplicate-process alarm
	// above it. A green box over a crashlooping cluster is the specific
	// output that hid daemon-gateway for an hour; the banner is the one
	// place someone is definitely looking at that moment.
	if cluster != nil && cluster.status == doctor.StatusFail {
		fmt.Fprintf(w, "%s ✗ CLUSTER WORKLOADS NOT RUNNING — this env is NOT up (see below)\n", bar)
	}
	printGroup("Host services", "host")
	printGroup("Frontends", "frontend")
	renderClusterWorkloads(w, bar, env, cluster)
	fmt.Fprintf(w, "%s\n", bar)
	fmt.Fprintf(w, "%s Logs   %s/   — tail -f / grep the per-service *.log here\n", bar, upLogDir(env))
	fmt.Fprintf(w, "%s Cluster routes:  forge cluster urls\n", bar)
	fmt.Fprintf(w, "%s Live status:     forge env status %s\n", bar, env)
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
	// Explicit contract first (same preference as hostEnvPorts): the first
	// declared listen port is the service's canonical/summary port. Falling
	// through to the env heuristic here would surface a port the service
	// never binds (e.g. a vestigial k8s-convention PORT), and the summary /
	// status probe would then report whatever foreign process holds it.
	if host.ListenPorts != nil {
		// Declared — including declared EMPTY, which means "binds nothing" and
		// must not fall through to the env heuristic (that would invent a port
		// for a service that has none, e.g. a packaged desktop app).
		if len(*host.ListenPorts) > 0 && (*host.ListenPorts)[0] > 0 {
			return strconv.Itoa((*host.ListenPorts)[0])
		}
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

// hostEnvPorts returns EVERY TCP port a host service will bind, derived
// from its inline env literals — not just the single canonical one
// hostEnvPort surfaces for the summary URL. A service commonly binds
// several ports (an API port, a metrics/pprof port, a debug port), each
// declared as its own `<...>_PORT` env var; probing only one of them let a
// real conflict slip past the pre-flight guard, so it launched a second
// stack on top of a stale one. This enumerates the full set instead.
//
// Ports come from:
//   - every `<...>_PORT`-suffixed env var with an inline value, and
//   - the generic `PORT`, but ONLY when the service declares no
//     service-specific `<NAME>_PORT` — a service that declares both binds
//     the specific one and treats generic PORT as a vestigial default (the
//     same heuristic hostEnvPort uses). Including a vestigial PORT would
//     over-detect: a false pre-flight conflict, or a readiness gate waiting
//     for a port the binary never binds.
//
// Only the inline `value` channel is visible host-side; config_map_ref /
// secret_ref ports carry no literal here and cannot be probed. Returned in
// declaration order, deduplicated. Empty when nothing declares a port.
func hostEnvPorts(name string, host *HostDeploy) []int {
	if host == nil {
		return nil
	}
	// Explicit contract first: a service that declares listen_ports has
	// stated exactly which host ports it binds — trust it and skip the env
	// heuristic below entirely. The heuristic sweeps EVERY *_PORT env var,
	// which misclassifies dependency-address vars (TEMPORAL_PORT,
	// WORKSPACE_URL_PORT, a leftover k8s-convention PORT) as bind ports and
	// then refuses `up` because healthy infra (docker temporal, the k3d
	// gateway LB) legitimately holds them.
	// nil means "not declared" (infer below); non-nil means the KCL stated the
	// set exactly — an EMPTY set is a legitimate statement that this service
	// binds no port at all, and must return empty rather than fall through.
	if host.ListenPorts != nil {
		var declared []int
		seenDeclared := map[int]bool{}
		for _, p := range *host.ListenPorts {
			if p <= 0 || p >= 65536 || seenDeclared[p] {
				continue
			}
			seenDeclared[p] = true
			declared = append(declared, p)
		}
		return declared
	}
	specific := strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_PORT"
	var ports []int
	seen := map[int]bool{}
	add := func(v string) {
		p, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || p <= 0 || seen[p] {
			return
		}
		seen[p] = true
		ports = append(ports, p)
	}
	hasSpecific := false
	generic := ""
	for _, ev := range host.EnvVars {
		if ev.Value == "" {
			continue
		}
		switch {
		case ev.Name == specific:
			hasSpecific = true
			add(ev.Value)
		case ev.Name == "PORT":
			generic = ev.Value // deferred: only the bind port when no <NAME>_PORT
		case strings.HasSuffix(ev.Name, "_PORT"):
			add(ev.Value)
		}
	}
	if !hasSpecific && generic != "" {
		add(generic)
	}
	return ports
}

// portConflict names a service/frontend the current `forge env up` would
// start whose expected listen port is already bound by something else.
type portConflict struct {
	name string
	port int
}

// portInUse reports whether something is already listening on
// 127.0.0.1:<port>. A successful TCP dial within a short timeout means a
// listener is accepting connections; "connection refused" (the common
// free-port case) returns false. Used by the `forge env up` pre-flight guard
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

// conflictingPorts computes the set of services/frontends THIS `forge env up`
// invocation would start whose expected listen port is already bound.
// It is the pure core of the pre-flight guard: probe is injected so the
// collection logic is testable without real sockets.
//
//   - Host services (deploy.Type=="host"): EVERY expected bind port via
//     hostEnvPorts (skipped when the service declares no inline port). A
//     multi-port service emits one conflict per busy port, so a collision
//     on any of its ports is caught — probing only one used to miss it.
//   - Frontends: fe.Port (skipped when 0, and entirely when frontendsOn
//     is false — the frontend feature is gated off).
//
// Only entities in the --target set are checked (inTargetSet), so
// `forge env up --target reliant-web` next to a running admin-server is fine
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
		for _, port := range hostEnvPorts(svc.Name, svc.Deploy.Host) {
			if probe(port) {
				conflicts = append(conflicts, portConflict{name: svc.Name, port: port})
			}
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

// portReleaseGrace bounds how long upPreflight waits for the ports of a stack
// it just tore down to come free, and portReleasePollInterval is how often it
// re-probes within that window. The grace only ever elapses in full on a
// genuine conflict; a normal teardown clears in a few hundred milliseconds and
// the loop exits early. Vars, not consts, so tests can shrink them.
var (
	portReleaseGrace        = 5 * time.Second
	portReleasePollInterval = 150 * time.Millisecond
)

// awaitPortRelease re-probes an ALREADY-conflicting set of ports until they
// come free or the grace expires, returning whatever is still bound.
//
// It narrows the set each round rather than re-running the full
// conflictingPorts scan: the ports that matter are exactly the ones that
// already failed, and each probe costs a real dial timeout.
//
// The budget counts elapsed SLEEP, not wall clock — probe time is excluded.
// That keeps the loop drivable by an injected sleep (tests advance no real
// time), and the two agree closely in practice because these are loopback
// dials: a bound port connects immediately and a free one is refused
// immediately, so neither approaches portInUse's 300ms timeout.
//
// sleep is injected so tests drive the loop without real time.
func awaitPortRelease(conflicts []portConflict, probe func(int) bool,
	grace, interval time.Duration, sleep func(time.Duration),
) []portConflict {
	remaining := conflicts
	for waited := time.Duration(0); ; waited += interval {
		remaining = stillBoundPorts(remaining, probe)
		if len(remaining) == 0 || waited >= grace {
			return remaining
		}
		sleep(interval)
	}
}

// stillBoundPorts filters a conflict set down to those whose port is still
// bound, preserving order so the reported list stays stable across rounds.
func stillBoundPorts(conflicts []portConflict, probe func(int) bool) []portConflict {
	var out []portConflict
	for _, c := range conflicts {
		if probe(c.port) {
			out = append(out, c)
		}
	}
	return out
}

// portReadyState is how a host service's expected bind port resolved on a
// post-launch readiness check.
type portReadyState int

const (
	// portReadyOurs: our own forge-up-marked child is listening — or (the
	// degrade case) SOMETHING is listening but the holder pid can't be
	// resolved (no lsof / Windows), so we can't prove it's foreign and don't
	// misfire. Either way the port is up and this run is not blocked.
	portReadyOurs portReadyState = iota
	// portReadyForeign: a resolvable, NON-forge-owned process holds the
	// port — a stale/old/foreign listener answering the probe, not the child
	// this run started. The "stale process painted green" trap.
	portReadyForeign
	// portReadyNobody: nothing is listening — our child never bound it (a
	// silent bind failure, or still mid-startup when the grace window ended).
	portReadyNobody
)

// classifyPortReadiness resolves who — if anyone — is listening on port and
// whether it is a child THIS env's `forge env up` started. listening / resolvePID
// are injected (portInUse / portListenerPID in production) so the classifier
// is unit-testable without real sockets or an lsof shell-out.
//
// It reuses the reclaim guard's ownership resolver (forgeOwnerOfPID): a
// listener whose ancestry carries FORGE_UP_ENV==env is ours. Because the
// pre-flight guard has already reclaimed (or errored on) any SAME-env orphan
// before we start, a marked listener on our port at this point is the child
// we just launched — not a prior run's orphan.
func classifyPortReadiness(port int, projectID, envName string, listening func(int) bool, resolvePID func(int) int, f procFacts) portReadyState {
	if !listening(port) {
		return portReadyNobody
	}
	pid := resolvePID(port)
	if pid <= 0 {
		// Something is listening but the holder is unidentifiable (lsof
		// missing / Windows no-op). Degrade to "up" rather than misfire — we
		// cannot prove a foreign holder. Mirrors enrichOwnership.
		return portReadyOurs
	}
	if _, ok := forgeOwnerOfPID(pid, projectID, envName, f); ok {
		return portReadyOurs
	}
	return portReadyForeign
}

// hostReadyResult is one expected host-service bind port and how it resolved.
type hostReadyResult struct {
	name  string
	port  int
	state portReadyState
	// holderPID / holderCmd identify the process actually on the port when
	// state is portReadyForeign. Populated only for that state — for the
	// others there is either no holder or the holder is our own child.
	//
	// forge already resolves the PID to decide ownership, so NOT reporting it
	// meant the message could say "held by another process" and then send the
	// reader to lsof to rediscover a fact forge had in hand. Observed
	// 2026-08-24: a stale `kubectl port-forward` to prod, running in another
	// terminal, held :3000 and blocked `forge env up dev`.
	holderPID int
	holderCmd string
}

// evalHostReadiness classifies EVERY expected bind port of the host services
// THIS run started (scoped by --target), once, using the injected probes.
// Pure + snapshot-based → unit-testable; waitHostServicesReady calls it each
// poll with live probes + a fresh process snapshot. Frontends are out of
// scope (their ports are resolve_port-dynamic); this gate covers the fixed
// host-service bind ports the incident was about.
func evalHostReadiness(e *KCLEntities, projectID, envName string, targets []string, listening func(int) bool, resolvePID func(int) int, f procFacts) []hostReadyResult {
	if e == nil {
		return nil
	}
	var out []hostReadyResult
	for _, svc := range e.Services {
		if svc.Deploy.Type != "host" || svc.Deploy.Host == nil {
			continue
		}
		if !inTargetSet(targets, svc.Name) {
			continue
		}
		for _, port := range hostEnvPorts(svc.Name, svc.Deploy.Host) {
			row := hostReadyResult{
				name:  svc.Name,
				port:  port,
				state: classifyPortReadiness(port, projectID, envName, listening, resolvePID, f),
			}
			// Identify the squatter while the probes are still in hand. Purely
			// additive: an unreadable argv leaves holderCmd empty and the
			// message falls back to what it always said.
			if row.state == portReadyForeign && resolvePID != nil {
				if pid := resolvePID(port); pid > 0 {
					row.holderPID = pid
					if f != nil {
						if args, ok := f.argv(pid); ok && len(args) > 0 {
							row.holderCmd = strings.Join(args, " ")
						}
					}
				}
			}
			out = append(out, row)
		}
	}
	return out
}

// hostReadyUnready filters a readiness snapshot to the ports NOT yet bound by
// our own child (foreign holder or nothing listening) — the set that keeps
// the poll loop going and, at timeout, names the failure.
func hostReadyUnready(rs []hostReadyResult) []hostReadyResult {
	var out []hostReadyResult
	for _, r := range rs {
		if r.state != portReadyOurs {
			out = append(out, r)
		}
	}
	return out
}

// hostReadyError renders the loud failure when the grace window ends with
// host-service ports still not bound by this run's child. It DETECTS ONLY —
// nothing is killed here; the started children are already in the ledger, so
// `forge env down` (which the message points at) reaches them.
func hostReadyError(envName string, unready []hostReadyResult, e *KCLEntities) error {
	var b strings.Builder
	fmt.Fprintf(&b, "[up] host service(s) never came up under this run (env=%s):\n", envName)
	for _, r := range unready {
		reason := "nothing is listening — the service failed to bind its port"
		if r.state == portReadyForeign {
			reason = "held by another process — not the child this run started (stale/foreign holder)"
		}
		fmt.Fprintf(&b, "       %-14s :%d  %s\n", r.name, r.port, reason)
		if r.holderPID > 0 {
			fmt.Fprintf(&b, "       %-14s   holder: pid %d%s\n", "", r.holderPID, holderCmdSuffix(r.holderCmd))
		}
		// A service can be declared against a port that a DIFFERENT component
		// actually binds — an Electron shell pointed at its web dev server is
		// the common shape. Blaming the shell for "failing to bind its port"
		// then sends the reader after the wrong process entirely, so name who
		// else declared it.
		if r.state != portReadyForeign {
			if others := portAlsoDeclaredBy(e, r.port, r.name); len(others) > 0 {
				fmt.Fprintf(&b, "       %-14s   note: %s also declares :%d — if it failed to start, nothing was ever going to bind this port\n",
					"", strings.Join(others, ", "), r.port)
			}
		}
	}
	fmt.Fprintf(&b, "     inspect the log under %s/ (lsof -i :<port> for the holder),\n", upLogDir(envName))
	fmt.Fprintf(&b, "     stop what this run started with: forge env down %s", envName)
	return errors.New(b.String())
}

// portAlsoDeclaredBy names the other declared services/frontends that claim
// this port, excluding the one being reported. Used to explain a "nothing is
// listening" verdict against a component that never binds the port itself.
func portAlsoDeclaredBy(e *KCLEntities, port int, except string) []string {
	if e == nil || port == 0 {
		return nil
	}
	var out []string
	for _, fe := range e.Frontends {
		if fe.Name != except && fe.Port == port {
			out = append(out, fmt.Sprintf("frontend %q", fe.Name))
		}
	}
	for _, svc := range e.Services {
		if svc.Name == except {
			continue
		}
		for _, p := range hostEnvPorts(svc.Name, svc.Deploy.Host) {
			if p == port {
				out = append(out, fmt.Sprintf("service %q", svc.Name))
				break
			}
		}
	}
	return out
}

// holderCmdSuffix renders a foreign holder's command line for the readiness
// error, truncated so one squatter cannot swamp the message. Empty when argv
// was unreadable — the pid alone is still enough to find the process.
func holderCmdSuffix(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	// Cut on a RUNE boundary. Slicing at a BYTE index splits any multi-byte
	// rune straddling the cut — one accented character in the holder's path is
	// enough — and the message then carries invalid UTF-8 (observed:
	// "/Users/jos\xc3…"), which terminals render as U+FFFD. The byte-length
	// test stays as the cheap pre-check: a string of <=max bytes can never
	// exceed max runes, so the conversion only runs on a line that may
	// genuinely need cutting.
	const max = 120
	if len(cmd) > max {
		if r := []rune(cmd); len(r) > max {
			cmd = string(r[:max]) + "…"
		}
	}
	return "  " + cmd
}

// hostReadyTimeout / hostReadyPoll bound the post-launch readiness gate: how
// long to wait for host services to bind, and how often to re-check. Air /
// go-run compile the binary on start, so the window must outlast a warm
// incremental build's compile+bind; a genuinely cold first build can exceed
// it, in which case the gate reports "nothing listening" for a service that
// was merely slow — the accepted trade for catching the silent-bind and
// stale-holder failures the summary used to paint green.
const (
	hostReadyTimeout = 15 * time.Second
	hostReadyPoll    = 250 * time.Millisecond
)

// waitHostServicesReady is the post-launch readiness gate. After the host
// phase has forked its runners, it polls every expected host-service bind
// port until OUR OWN marked child is the listener on each, or the grace
// window elapses — then errors, naming the offending service/port(s). This
// closes the gap where forge only confirmed the runner FORKED (never that
// its child actually bound the port) and the best-effort summary could not
// tell "my new child bound it" from "a stale holder still answers" — so it
// painted a stale process green. A fresh process snapshot is taken each poll
// so an air re-exec / late fork is picked up. Nothing is killed: detect and
// report only. A nil return means every declared host port is bound by us
// (or nothing declared a port).
func waitHostServicesReady(e *KCLEntities, projectID, envName string, targets []string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		rs := evalHostReadiness(e, projectID, envName, targets, portInUse, portListenerPID, newOSProcFacts())
		if len(rs) == 0 {
			return nil // no host service declares a bind port; nothing to gate
		}
		unready := hostReadyUnready(rs)
		if len(unready) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return hostReadyError(envName, unready, e)
		}
		time.Sleep(poll)
	}
}

// ── the CLUSTER half of the post-launch readiness gate ───────────────────
//
// waitHostServicesReady above is the host half, and until now it was the
// ONLY half: `forge env up` blocked on host-service port binds, printed a
// green summary box, and exited 0 while the Kubernetes workloads it had just
// applied were crashlooping. Its failure message says as much verbatim —
// "host service(s) never came up under this run". Measured on control-plane
// in one week: `forge env up dev` reported every service `up` and exited 0
// while daemon-gateway was OOMKilled and CrashLoopBackOff (the product said
// "no machine is connected" and nothing in forge contradicted the banner),
// and separately five pods — admin-api in CreateContainerConfigError, two
// litellm and two reliant-api-server at ~120 and ~130 restarts — crashlooped
// for TEN HOURS behind that same green output.
//
// The judgement is NOT re-derived here. doctor.CheckClusterWorkloads already
// owns "is this env's rendered workload set alive", including the startup
// grace that separates a slow first rollout from an outage, the hard waiting
// reasons that are failures on sight, and the three-state Skip / Unknown /
// Pass-Fail model. This is a CALL SITE: run it once, print it in the box,
// and let its Status decide the exit code. One definition of "broken", so
// `forge env up` and `forge env status` can never disagree about it.

const (
	// clusterHealthTimeout bounds the whole cluster-workload assertion. The
	// check bounds itself (10s around the KCL render, 6s per kubectl call,
	// probes concurrent), so this is the belt to that braces: a hung render
	// plugin or a kubectl that ignores its own deadline must not turn the
	// last step of `forge env up` into a hang. Cancellation degrades to
	// UNDETERMINED inside the check, never to a failure.
	clusterHealthTimeout = 30 * time.Second
)

// clusterWorkloadCheck is the seam. Production is doctor's check; tests
// substitute a fabricated verdict, which is the only way to pin
// "crashlooping ⇒ non-zero exit" and "unreachable cluster ⇒ still succeeds"
// without a cluster to break.
var clusterWorkloadCheck = doctor.CheckClusterWorkloads

// clusterWorkloadSummary is the rolled-up cluster verdict, reduced to what
// the summary box prints and the exit code reads. A nil *clusterWorkloadSummary
// means "this run has nothing to say about a cluster" — the gate was off
// (deploy feature disabled, or a --target set with no cluster edge) or the
// check itself answered SKIP because the env deploys nothing to Kubernetes.
// Nil prints nothing and fails nothing.
type clusterWorkloadSummary struct {
	status doctor.Status
	// message is doctor's one-line verdict, already assembled to name the
	// pod, the container, the waiting reason and the OOMKill. It is carried
	// verbatim: shortening it here would recreate the "1 workload unhealthy"
	// report that cost an hour.
	message string
}

// upClusterGateEnabled reports whether this run should assert on cluster
// workload health at all.
//
// Two ways to be out of scope, and both are a deliberate statement by the
// caller that forge is not responsible for the cluster on this run:
//
//   - features.deploy off — the project has told forge it does not deploy.
//   - a `--target` set with no cluster edge (targeting only host services or
//     dev-served frontends). targetPhaseRequirements is reused rather than
//     re-derived: it is already the authority for "does this run need a
//     cluster", and a second copy of that rule would drift from the phase it
//     is supposed to describe.
//
// This is where `--target` replaced `--host-only`. A run that wants only the
// host side names the services it wants, and the cluster edge disappears from
// the target closure — so the gate goes quiet for exactly the same reason it
// used to, but derived from what the run actually selected rather than from a
// flag asserting it.
//
// Note what is NOT here. `--no-deploy` is deliberately not an arm: it means
// "skip the apply", not "don't care", and a run leaning on an
// already-deployed cluster is exactly the one that most needs to hear that
// the cluster is broken.
func upClusterGateEnabled(store featureReader, e *KCLEntities, targets []string) bool {
	if !isFeatureEnabled(store, config.FeatureDeploy) {
		return false
	}
	return targetPhaseRequirements(e, targets).cluster
}

// evalClusterWorkloads runs the cluster verdict ONCE — a single assertion,
// not a poll.
//
// Polling would be both wasteful and wrong. The waiting already happened:
// the deploy phase runs `kubectl rollout status --timeout=60s` per managed
// Deployment and waits every one-shot Job to completion (cluster.Apply), so
// by the time control reaches here each workload has had its rollout window
// and — on the full path — the whole host phase on top of it. And the check
// carries a 90s startup grace that reports a young, not-yet-Ready pod as a
// WARNING rather than a failure; polling that at 250ms would be waiting for
// a pod to grow old enough to condemn, which is precisely backwards. One
// look, ~450-600ms, at the latest moment the answer can be taken.
//
// Returns nil for "nothing to say" so every downstream site — the box, the
// exit code — has one thing to test.
func evalClusterWorkloads(ctx context.Context, enabled bool, projectDir, env string) *clusterWorkloadSummary {
	if !enabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, clusterHealthTimeout)
	defer cancel()
	res := clusterWorkloadCheck(ctx, &doctor.Environment{ProjectDir: projectDir, Env: env})
	if res.Status == doctor.StatusSkip {
		// NOT APPLICABLE — this env deploys nothing to Kubernetes. Distinct
		// from UNDETERMINED, which is carried through and printed.
		return nil
	}
	return &clusterWorkloadSummary{status: res.Status, message: res.Message}
}

// clusterWorkloadError is the exit-code decision, and it is deliberately one
// line of policy: ONLY a Fail fails the command.
//
//   - Fail — a hard waiting reason (CrashLoopBackOff, CreateContainerConfigError,
//     ImagePullBackOff…), an OOMKill on a pod that is down, a workload the
//     render declares with no pods at all, or a pod still not Ready past the
//     startup grace. This is the whole point: exit non-zero.
//   - Warn — mid-rollout, a DaemonSet with no matching node, a pod that is
//     serving but has been OOM-killed or keeps restarting. Printed, never
//     fatal. Failing a slow first rollout would be its own false alarm, and
//     a check that cries wolf is the state forge was already in.
//   - Unknown — cluster unreachable, kubectl missing, RBAC refused, render
//     failed, timeout. Reported honestly and NEVER fatal. A developer with
//     no cluster must be able to bring their host stack up.
//
// Nothing is killed here, exactly as the host half does not kill: the
// children this run started are already in the ledger and reachable by
// `forge env down`.
func clusterWorkloadError(env string, s *clusterWorkloadSummary) error {
	if s == nil || s.status != doctor.StatusFail {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[up] cluster workload(s) never came up under this run (env=%s):\n", env)
	fmt.Fprintf(&b, "       %s\n", s.message)
	fmt.Fprintf(&b, "     forge applied these manifests and waited on their rollouts; they are still not running.\n")
	fmt.Fprintf(&b, "     per-pod detail: forge env status %s -v", env)
	return errors.New(b.String())
}

// clusterStatusGlyph is the summary box's mark for the cluster verdict.
// Pass borrows the box's own "up" dot so the healthy case reads as one
// table; the abnormal states borrow `forge doctor`'s marks so the same fact
// looks the same in both commands. UNDETERMINED gets "?" and never the
// gray dash — "I could not tell" must not render as "does not apply".
func clusterStatusGlyph(s doctor.Status) string {
	switch s {
	case doctor.StatusPass:
		return "●"
	case doctor.StatusFail:
		return "✗"
	case doctor.StatusWarn:
		return "!"
	default:
		return "?"
	}
}

// renderClusterWorkloads writes the box's "Cluster workloads" group — the
// third group, peer to Host services and Frontends. The box being silent
// about the cluster was the same defect one layer up from the exit code: it
// claimed to show what `forge env up` brought up while showing only the half
// running on the developer's machine.
func renderClusterWorkloads(w io.Writer, bar, env string, s *clusterWorkloadSummary) {
	if s == nil {
		return
	}
	fmt.Fprintf(w, "%s Cluster workloads\n", bar)
	fmt.Fprintf(w, "%s  %s %s\n", bar, clusterStatusGlyph(s.status), s.message)
	if s.status != doctor.StatusPass {
		// The message names the pod and the reason; evidence (the full
		// per-target roster) prints only under -v, and this is the pointer
		// to it. Not printed on a pass: there is nothing to go look at.
		fmt.Fprintf(w, "%s       ↳ forge env status %s -v\n", bar, env)
	}
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
func upBuildCluster(ctx context.Context, _ *config.ProjectConfig, env string, noGenerate bool, targets []string) error {
	registry := "localhost:5050"
	if reg := k8sClusterRegistryForEnv(ctx, env); reg != "" {
		registry = reg
	}
	return runBuild(ctx, upBuildOptionsFor(env, registry, noGenerate, targets))
}

// upBuildOptionsFor is the pure construction of `forge env up`'s build-phase
// options, split out from upBuildCluster so the wiring is unit-testable
// without a cluster to query for a registry. targets is the load-bearing
// field: it is what scopes the docker build+push to the apps this run is
// actually bringing up, so a `--target <frontend>` builds no images at all.
func upBuildOptionsFor(env, registry string, noGenerate bool, targets []string) buildOptions {
	return buildOptions{
		outputDir:     "bin",
		buildTarget:   "all",
		parallel:      true,
		buildDocker:   true,
		pushRegistry:  registry,
		env:           env,
		skipFrontends: true,
		skipGenerate:  noGenerate,
		targets:       targets,
	}
}

// prewarmInfra brings up the project's declared dev INFRASTRUCTURE — the
// servers the app dials but does not build — before any host process tries
// to connect to one.
//
// It dispatches whatever the env declares, through the same
// buildDeployGroups → Provider.Deploy path the deploy phase takes, so the
// deploy phase's later re-dispatch is a true idempotent no-op rather than a
// second, subtly-different code path. Two providers are infrastructure:
//
//   - host-infra — a server forge runs as a HOST PROCESS (postgres, via
//     internal/hostinfra). This is the scaffolded default: a dev loop whose
//     own code needs no container should not need a container runtime, and
//     on a small cloud VM docker is a cost paid before the project starts.
//   - compose — a container, for a project that declares one. The dev IdP
//     is still one of these (Zitadel is a server with its own postgres
//     dependency; running it host-native is a different project), so a
//     project WITH a frontend does still need docker for that one service.
//
// Namespace is irrelevant to both, so the group builder gets an empty
// fallback.
//
// EVERY group is attempted, even after one fails, and the failures are
// reported together. The caller already treats this whole call as
// best-effort (see the "continuing; host services may fail to connect" log
// at the call site) — but a single `return` on the first group's error used
// to abandon every group after it, silently. A dev stack's compose services
// share one file and therefore one group, so a port collision on postgres
// took the IdP down with it, and the failure surfaced three steps
// downstream as a job reading a PAT file the IdP never got to write, with
// nothing connecting the two. Collecting every group's error names the
// actual cause at the actual point of failure.
func prewarmInfra(ctx context.Context, env string, entities *KCLEntities) error {
	groups, err := buildDeployGroups(env, entities, "")
	if err != nil {
		return fmt.Errorf("group infrastructure services: %w", err)
	}
	projectDir := projectDirForKCL()
	return deployInfraGroups(ctx, groups, map[string]deploytarget.Provider{
		"host-infra": deploytarget.HostInfraProvider{ProjectDir: projectDir},
		"compose":    deploytarget.ComposeProvider{ProjectDir: projectDir},
	})
}

// deployInfraGroups runs each INFRASTRUCTURE group through its provider and
// returns every failure, joined. Groups whose provider is not in the map
// (cluster / external / firebase) are skipped — they are applications, not
// the servers those applications dial.
//
// Split out from prewarmInfra so the attempt-everything contract is
// testable without a project on disk. That contract is the whole point of
// this function: the caller treats infra bring-up as best-effort, and a
// `return` on the first group's error silently abandoned every group after
// it — which is how one port collision used to take an entire dev stack
// down while naming only the first casualty.
func deployInfraGroups(ctx context.Context, groups []deploytarget.ServiceGroup, providers map[string]deploytarget.Provider) error {
	var failures []error
	for _, g := range groups {
		provider, isInfra := providers[g.ProviderID]
		if !isInfra {
			continue
		}
		if err := provider.Deploy(ctx, g); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", infraGroupServiceNames(g), err))
		}
	}
	return errors.Join(failures...)
}

// infraGroupServiceNames names the services in an infrastructure deploy
// group, for an error message that says WHICH services a failed group left
// undeployed — a human reading "compose: exit status 1" has no way to know
// which of the services sharing that group never came up.
func infraGroupServiceNames(g deploytarget.ServiceGroup) string {
	names := make([]string, 0, len(g.Services))
	for _, svc := range g.Services {
		names = append(names, svc.Name)
	}
	return strings.Join(names, ", ")
}

// reconcileCluster is the cluster-scope reconcile entry point shared by
// `forge env deploy` and `forge env up`'s deploy phase: render the env's KCL,
// resolve context / namespace / secrets, and apply the in-cluster
// workloads + External/Compose deploy targets. opts carries the surgical
// knobs (tag / rollback / prune / dry-run / context override / targets /
// skip-frontend); `forge env deploy` fills it from its flags, `forge env up` passes
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

// reportEnvConflicts prints every declared variable whose value differed from
// one already in the developer's shell. Silence here is what made a broken
// stack unexplainable: the project declares one value, the process runs with
// another, and nothing says so. Ports have been reported this way for a while
// (forceHostBindPorts); this extends it to every declared key.
func reportEnvConflicts(service string, conflicts []envutil.EnvConflict) {
	for _, c := range conflicts {
		if c.ShellWon {
			fmt.Printf("  %s: %s taken from your shell (%s=%s), overriding the declared value — %s names it\n",
				service, c.Key, hostlaunch.ShellWinsEnvVar, c.Key, hostlaunch.ShellWinsEnvVar)
			continue
		}
		fmt.Printf("  %s: %s uses the DECLARED value, ignoring a different one in your shell (export %s=%s to prefer the shell)\n",
			service, c.Key, hostlaunch.ShellWinsEnvVar, c.Key)
	}
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
// Unlike `forge run <svc>`, `forge env up` is strict about unknown runners:
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
	// os.Environ() wins last.
	//
	// Secrets come from the env's bundle secret_provider, SCOPED to what
	// this service DECLARES: the provider map is the whole store, and a
	// service receives only the keys it names via EnvVar.secret_ref.
	// Injecting the whole store was how an undeclared value reached every
	// process — which made dropping a line in the secrets file, rather
	// than declaring it in KCL, the path of least resistance, and is why
	// non-secret config drifted out of version control. An undeclared
	// secret now reaches nothing.
	//
	// (The legacy per-service `secrets_file` dotenv fallback was removed
	// with the dotenv provider: it was the same wholesale injection with a
	// per-service path.)
	secretVals := scopeSecretsToService(secretsLayer, &svc)
	envVars := hostEnvVarsToMap(host)
	// projectConfig layer: forge.yaml environments[<env>].config projected
	// to env-var strings. Same source the cluster ConfigMap projection
	// uses; layering it here keeps `forge env up` host services in sync with
	// `forge run <svc>`. nil cfg / empty env collapses to an empty map.
	var projectConfigEnv map[string]string
	if cfg != nil && env != "" {
		projectConfigEnv = loadProjectConfigEnv(cfg, env)
	}
	// Dev-run defaults: on a dev env, `forge run` marks the runtime as
	// development AND auto-applies migrations on boot, so a fresh dev DB
	// comes up with its schema without any hand-set env vars. Authentication
	// is untouched here — a real validator is built in every mode. Lowest
	// precedence — overridden by project config, secrets,
	// KCL env_vars, and the shell (see withDevRunDefaults). The env is
	// classified via the same config source the seed gate reads.
	dev, _ := seedTargetIsDev(env)
	projectConfigEnv = withDevRunDefaults(projectConfigEnv, dev)
	hostEnv, envConflicts := hostlaunch.LayerHostEnvConflicts(os.Environ(), projectConfigEnv, secretVals, envVars)
	reportEnvConflicts(svc.Name, envConflicts)
	cmd.Env = hostEnv
	cmd.Env = forceHostBindPorts(cmd.Env, svc.Name, envVars)

	return cmd, svc.Name, nil
}

// forceHostBindPorts makes the port the process BINDS the same port forge
// PUBLISHED. Every other host env var follows the shell-wins policy
// LayerHostEnv implements — a dev overriding DATABASE_URL from their shell is
// the point. A bind port cannot work that way, because forge is not the only
// reader: the pre-flight port-conflict guard, the readiness gate, the summary
// URL and the frontend's inlined API URL all read the KCL/allocated value,
// while only the process reads the environment. Letting an inherited PORT win
// desynchronizes all five — forge prints and wires one port and the app
// answers on another, so the frontend bundle points at a dead port and the
// readiness probe passes against whatever else holds the published one.
//
// Only the bind-port names are forced: the generic PORT and any
// `<...>_PORT`-suffixed var forge declared inline, i.e. exactly the set
// hostEnvPorts treats as ports this service binds. A shell value that is
// being overridden is reported rather than dropped in silence.
func forceHostBindPorts(env []string, svcName string, declared map[string]string) []string {
	inherited := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			inherited[kv[:i]] = kv[i+1:]
		}
	}
	for _, name := range sortedKeys(declared) {
		if name != "PORT" && !strings.HasSuffix(name, "_PORT") {
			continue
		}
		value := declared[name]
		if was, ok := inherited[name]; ok && was != value {
			fmt.Printf("[up] host %s: %s=%s in the environment ignored — forge publishes %s and the app must bind it\n",
				svcName, name, was, value)
		}
		env = withForcedEnv(env, name, value)
	}
	return env
}

// withDevRunDefaults layers the dev-run environment UNDER the project config
// when isDev, so `forge run` / `forge env up dev` boots a fresh dev app
// turnkey with zero hand-set env vars:
//
//   - ENVIRONMENT=development — marks the runtime as development so dev
//     ergonomics (permissive CORS, verbose errors) apply. Authentication is
//     still enforced: SetupAuth builds a real validator in every mode.
//
//   - AUTO_MIGRATE=true — the app applies its migrations on boot, so a
//     freshly-created dev DB has its schema before the host-services
//     readiness gate + first-boot auto-seed run (maybeAutoSeed assumes the
//     schema is current — it seeds, it does not migrate). Without this a
//     `forge run` against an empty DB serves an unmigrated, tableless app.
//
// CORS needs no entry here. ENVIRONMENT=development is itself what enables
// the backend's CORS layer (serverkit.Config.CORSEnabled) and selects the
// origin-reflecting dev policy, so the dashboard reaches its own API no
// matter which port the frontend landed on — including a kernel-assigned
// ephemeral one, which no derived allow-list could have named in advance.
//
// It is the LOWEST-precedence layer: project config (returned on top here),
// then secrets, KCL env_vars, and the developer's shell all override it (see
// hostlaunch.LayerHostEnv). When isDev is false (prod/staging) the project
// config passes through untouched, so production behavior is unchanged — a
// prod deploy still owns its migration story (a Job/initContainer), never an
// implicit on-boot migrate, and its CORS allow-list stays explicit.
//
// An EMPTY project-config value never shadows a dev default. The KCL
// projection is total — config_gen.k emits one entry per AppConfig
// field unconditionally (`"CORS_ORIGINS" = {value = c.cors_origins}`), so a
// field the env's config.k never pinned still arrives here carrying its
// schema zero value. That "" is an ABSENCE, not a decision, and it is
// indistinguishable from one at this layer; letting it win would make the
// dev defaults dead code for every field the scaffold doesn't pin. ENVIRONMENT
// is the field that makes this rule load-bearing: it is what puts the backend
// in the development posture, and an unpinned "" arriving from the total
// projection would silently demote a `forge run` to a deployed posture — a
// backend that then refuses its own frontend's preflight, so every CRUD page
// renders "Couldn't load data / Failed to fetch" against a seeded DB.
// Pin a field in deploy/kcl/<env>/config.k to override for real.
func withDevRunDefaults(projectConfigEnv map[string]string, isDev bool) map[string]string {
	if !isDev {
		return projectConfigEnv
	}
	out := map[string]string{
		"ENVIRONMENT":  devModeValue,
		"AUTO_MIGRATE": "true",
	}
	// Project config overrides the dev-run defaults on key conflict — but only
	// with a value that says something. See the doc comment.
	for k, v := range projectConfigEnv {
		if v == "" {
			if _, defaulted := out[k]; defaulted {
				continue
			}
		}
		out[k] = v
	}
	return out
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
// frontendLaunch carries the inputs for the frontend half of `forge env up`.
// A struct rather than nine positionals — `background, noInstall bool` and
// `targets, frontendArgs []string` are adjacent same-typed pairs, the exact
// shape a caller silently transposes.
type frontendLaunch struct {
	entities     *KCLEntities
	env          string
	background   bool
	noInstall    bool
	targets      []string
	frontendArgs []string
	apiBaseURL   string
	procs        *procRegistry
}

// frontendPhaseEnabled reports whether `forge env up` should start the
// frontends THIS render declares.
//
// features.frontend is an explicit OPT-OUT, not a shape signal. Its derived
// default comes from DeriveFeatureDefaults, which answers "does this project
// have frontends?" by reading forge.yaml's `frontends:` inventory — and that
// inventory is no longer where frontend topology lives. A project that
// declares its frontends in deploy/kcl/<env>/ only (the supported shape:
// `forge build --target <frontend>` and the deploy path both resolve them
// from the render) therefore derives features.frontend = false, and gating
// the phase on that derived default skipped every declared frontend while
// printing "feature 'frontend' is disabled in forge.yaml" — about a file that
// says nothing of the sort. The stack came up with no dev server, and the
// only loud symptom was some OTHER service that waits on a frontend port.
//
// So the question is asked of the two sources that can actually answer it:
// an explicit forge.yaml value wins in BOTH directions (a project that turns
// the feature off gets no frontend phase, one that turns it on keeps it),
// and with no explicit value the RENDER decides — frontends declared means
// frontends started.
func frontendPhaseEnabled(store featureReader, e *KCLEntities) bool {
	if store != nil {
		if f := store.Features(); featureExplicitlySet(f, config.FeatureFrontend) {
			return f.FrontendEnabled()
		}
	}
	return e != nil && len(e.Frontends) > 0
}

func upFrontends(ctx context.Context, fl frontendLaunch) int {
	e, env, background, noInstall := fl.entities, fl.env, fl.background, fl.noInstall
	targets, frontendArgs, apiBaseURL, procs := fl.targets, fl.frontendArgs, fl.apiBaseURL, fl.procs
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
		cmd := buildFrontendCmd(ctx, fe, env, os.Environ(), frontendArgs, apiBaseURL)
		if err := procs.start("frontend:"+fe.Name, cmd, background); err != nil {
			fmt.Printf("[up] frontend %s: %v\n", fe.Name, err)
			failures++
		}
	}
	return failures
}

// preflightFrontendDeps installs every selected frontend's dependencies before
// the expensive phases run. Returns the FIRST failure, so the run stops while
// the user is still watching rather than after a full build.
func preflightFrontendDeps(ctx context.Context, e *KCLEntities, targets []string) error {
	if e == nil {
		return nil
	}
	var selected []FrontendEntity
	for _, fe := range e.Frontends {
		if len(targets) > 0 && !slices.Contains(targets, fe.Name) {
			continue
		}
		if fe.Path == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(fe.Path, "package.json")); err != nil {
			continue
		}
		if frontendDepsStale(fe.Path) {
			selected = append(selected, fe)
		}
	}
	if len(selected) == 0 {
		return nil // warm run: nothing stale, nothing printed
	}
	fmt.Println("\n[up] frontend dependency phase (before build — a dep failure should not cost a build)")
	for _, fe := range selected {
		if err := ensureFrontendDeps(ctx, fe, false); err != nil {
			return fmt.Errorf("frontend %s: %w", fe.Name, err)
		}
	}
	return nil
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
	err := runFrontendInstall(ctx, runner, fe.Path)
	if err != nil && transientInstallFailure(err.Error()) {
		// The package manager reported ITS OWN internal failure, not a problem
		// with the tree — retrying converges where failing the whole `up` does
		// not. Without this a known npm bug ends a run that had already spent
		// minutes building images, and re-running hits the same coin flip.
		fmt.Printf("[up] %s: `%s install` hit a package-manager internal error — retrying once\n", fe.Name, runner)
		err = runFrontendInstall(ctx, runner, fe.Path)
	}
	if err != nil {
		return fmt.Errorf("install deps in %s: %w", fe.Path, err)
	}
	markFrontendInstallOK(fe.Path)
	return nil
}

// proxiedInstallSockets caps a package manager's parallel connections when the
// install runs through an HTTP proxy.
//
// Package managers open many sockets at once — npm's default is 15 — which a
// local debugging/inspection proxy (Proxyman, Charles, mitmproxy) does not
// survive: measured on a 579-package tree, 15 sockets through such a proxy did
// not finish in FOUR MINUTES, while 8 finished in 7.6s against 6.6s direct.
// The failure is also silent, because npm's fetch-timeout is 300s with 2
// retries: the install simply sits there for up to fifteen minutes looking
// hung, and the eventual error names nothing useful.
//
// 8 is chosen with margin: 10 also measured clean, 15 did not, so the cliff
// sits between them. The cost when proxied is ~1s on that tree; the cost when
// NOT proxied is zero, because the cap is only applied when a proxy is set.
const proxiedInstallSockets = 8

// installConcurrencyFlags caps network concurrency for this runner, but only
// when the environment actually routes through a proxy. Returning nil in the
// common case keeps a normal install at full speed.
func installConcurrencyFlags(runner string, env []string) []string {
	proxied := false
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if envutil.Lookup(env, key) != "" {
			proxied = true
			break
		}
	}
	if !proxied {
		return nil
	}
	switch runner {
	case "npm":
		return []string{fmt.Sprintf("--maxsockets=%d", proxiedInstallSockets)}
	case "pnpm", "yarn":
		return []string{fmt.Sprintf("--network-concurrency=%d", proxiedInstallSockets)}
	default:
		return nil
	}
}

// runFrontendInstall runs one install attempt, teeing output to the terminal
// while retaining it so the caller can classify the failure.
func runFrontendInstall(ctx context.Context, runner, dir string) error {
	var buf bytes.Buffer
	args := []string{"install"}
	if flags := installConcurrencyFlags(runner, os.Environ()); len(flags) > 0 {
		args = append(args, flags...)
		fmt.Printf("[up] proxy detected — capping %s network concurrency at %d (an inspection proxy stalls at the default)\n",
			runner, proxiedInstallSockets)
	}
	cmd := exec.CommandContext(ctx, runner, args...)
	cmd.Dir = dir
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(buf.String()))
	}
	return nil
}

// transientInstallFailureSignatures are messages by which a package manager
// reports a fault in ITSELF rather than in the project. They are worth exactly
// one retry: the tree is fine, the tool tripped.
var transientInstallFailureSignatures = []string{
	// npm's own internal-error marker; it prints "This is an error with npm
	// itself" and asks the user to file a bug. Frequently succeeds on retry.
	"Exit handler never called!",
	// Registry/network flakes that are equally not the project's fault.
	"ECONNRESET",
	"ETIMEDOUT",
	"ERR_SOCKET_TIMEOUT",
	"registry error",
}

func transientInstallFailure(output string) bool {
	for _, sig := range transientInstallFailureSignatures {
		if strings.Contains(output, sig) {
			return true
		}
	}
	return false
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
	// A SUCCESSFUL install is the reference point, not node_modules' mtime.
	// An install that fails part-way still writes packages, which bumps that
	// mtime — so the directory looks current, the next run skips the install,
	// and a half-populated tree is treated as done. That is the whole gate
	// inverting itself precisely when it matters. The stamp is written only
	// after the package manager exits 0.
	var ref time.Time
	if stamp, err := os.Stat(frontendInstallStamp(dir)); err == nil {
		ref = stamp.ModTime()
	} else {
		// No stamp: either a tree installed before forge wrote stamps, or one
		// left behind by a failed install. Fall back to the mtime heuristic so
		// an existing healthy checkout is not force-reinstalled.
		ref = nm.ModTime()
	}
	for _, manifest := range []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock", "package.json"} {
		if info, err := os.Stat(filepath.Join(dir, manifest)); err == nil {
			if info.ModTime().After(ref) {
				return true
			}
		}
	}
	return false
}

// frontendInstallStamp is the marker written after a package manager exits
// successfully, so "are these deps current?" asks about the last install that
// COMPLETED rather than the last one that merely ran.
func frontendInstallStamp(dir string) string {
	return filepath.Join(dir, "node_modules", ".forge-install-ok")
}

// markFrontendInstallOK records a completed install. Best-effort: a tree that
// cannot be stamped just falls back to the mtime heuristic next time.
func markFrontendInstallOK(dir string) {
	_ = os.WriteFile(frontendInstallStamp(dir), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
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
// fe.Port == 0 skips the force-inject so we don't surface a meaningless
// "PORT=0" line that would crash the dev server. In the normal flow an
// ephemeral (unset) frontend has already been assigned a concrete free port
// by resolveEphemeralFrontendPorts before we get here, so Port is > 0 and the
// dev server binds the port forge reported in the summary; Port stays 0 only
// when that allocation failed, in which case the dev server picks its own.
//
// frontendArgs are passthrough tokens forwarded to the dev server after a
// `--` separator (`npm run dev -- <frontendArgs>`), so `forge run --
// --host 0.0.0.0` reaches Vite/Next. Empty (the `forge env up` default) leaves
// the command untouched.
func buildFrontendCmd(ctx context.Context, fe FrontendEntity, env string, parentEnv, frontendArgs []string, apiBaseURL string) *exec.Cmd {
	runner := fe.DevRunner
	if runner == "" {
		runner = "npm"
	}
	runArgs := []string{"run", "dev"}
	if len(frontendArgs) > 0 {
		// `npm run dev -- <args>` forwards <args> to the underlying dev
		// server (vite / next dev) rather than to npm itself. The `--`
		// terminator is required for npm/pnpm/yarn to stop consuming flags.
		runArgs = append(runArgs, "--")
		runArgs = append(runArgs, frontendArgs...)
	}
	cmd := exec.CommandContext(ctx, runner, runArgs...)
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
	// Point the frontend at the backend's ACTUAL (ephemeral) dev port. The
	// backend no longer binds a fixed :8080 in host-only mode, so the value
	// forge baked into the frontend (DEV_API_URL) would be stale; inject the
	// type-appropriate API-URL env var the frontend's dev transport reads
	// (NEXT_PUBLIC_API_URL / VITE_API_URL / EXPO_PUBLIC_API_URL). Placed in
	// the LOWEST (env-file) layer so a developer's shell or an explicit KCL
	// env_var still overrides it. No-op for a full cluster up (apiBaseURL "").
	if apiBaseURL != "" {
		if envFileMap == nil {
			envFileMap = map[string]string{}
		}
		envFileMap[frontendAPIURLEnvVar(fe.Type)] = apiBaseURL
	}
	// EffectiveEnvVars folds the typed `config` block (mock/api_url/otel/
	// environment) into the KCL env stream, explicit env_vars winning on a
	// collision. Placed at the KCL layer (below parentEnv) so a developer's
	// shell still overrides — a dev can `NEXT_PUBLIC_MOCK_API=true npm run
	// dev` even when config.mock is off. A config-declared mock=true/hybrid
	// applies here as the dev-launch default.
	cmd.Env = hostlaunch.LayerHostEnv(parentEnv, envFileMap, nil, kclEnvVarsToMap(fe.EffectiveEnvVars()))

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
// `forge env down <env>` can find them.
type procRegistry struct {
	projectID  string
	projectDir string
	env        string
	mu         sync.Mutex
	processes  []*managedProcess
}

func newProcRegistry(projectID, projectDir, env string) *procRegistry {
	return &procRegistry{projectID: projectID, projectDir: projectDir, env: env}
}

// start launches a child command, captures stdout/stderr with a
// `[<name>]` prefix, and registers the process for later teardown.
// When background==true, stdout/stderr are sent to per-service log
// files under ~/.cache/forge/up/<env>/<name>.log so the user can
// `tail -f` them after detach.
func (p *procRegistry) start(name string, cmd *exec.Cmd, background bool) error {
	// Stamp forge ownership onto the child (and, via env inheritance, every
	// descendant) BEFORE it starts. This is the authoritative signal a later
	// `forge env up` uses to recognise its OWN orphans (this project + env) on
	// a busy port even when the .pids ledger has drifted — see up_reclaim.go.
	// The project marker is what keeps a different project's reclaim off this
	// child.
	stampForgeOwnership(cmd, p.projectID, p.env, name)
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
		// real PID so `forge env down` can later SIGTERM it.
		pid := 0
		if cmd.Process != nil {
			pid = cmd.Process.Pid
			_ = cmd.Process.Release()
		}
		p.mu.Lock()
		p.processes = append(p.processes, &managedProcess{name: name, cmd: cmd, pid: pid})
		p.mu.Unlock()
		p.persist()
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
	p.persist()
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

// persist writes the tracked PID set and this project's path record to disk.
// Each ledger line is `<name>\t<pid>`.
//
// It runs after EVERY start, not once at the end of the launch phases, because
// any early return between the two loses the record of children that are
// already running: a failed readiness gate used to return before the single
// end-of-run persist, leaving every started child alive with no ledger ever
// written and nothing on disk naming the project they belonged to. Rewriting a
// hundred bytes per start is cheaper than a class of unreachable orphans.
func (p *procRegistry) persist() {
	statePath, err := upStatePath(p.projectID, p.env)
	if err != nil {
		fmt.Printf("[up] warning: resolve state path: %v\n", err)
		return
	}
	// The path record is what makes this stack nameable by `forge env ps` and
	// stoppable by `forge env down --all` after the project directory is gone.
	recordProjectDir(p.projectID, p.projectDir)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		fmt.Printf("[up] warning: mkdir state: %v\n", err)
		return
	}
	// Entries this run did NOT start, carried forward. A scoped
	// (`--target`) run replaces only the services it names and deliberately
	// leaves the rest of the stack running (see stopStackScoped); those
	// processes are still live and must stay in the ledger, which is what
	// `forge env down` and `forge env ps` read to find them. Dead and
	// same-named entries are dropped — this run's PID is the current one,
	// and a process that has exited is not something to remember.
	//
	// No-op for a full run: the unscoped teardown removed the ledger before
	// this point, so there is nothing to carry.
	carried := make([]trackedProc, 0)
	started := make(map[string]bool, len(p.processes))
	p.mu.Lock()
	for _, mp := range p.processes {
		started[mp.name] = true
	}
	p.mu.Unlock()
	for _, prev := range trackedStack(p.projectID, p.env) {
		if started[prev.name] || prev.pid <= 0 || !processAlive(prev.pid) {
			continue
		}
		carried = append(carried, prev)
	}

	var b strings.Builder
	for _, prev := range carried {
		_, _ = fmt.Fprintf(&b, "%s\t%d\n", prev.name, prev.pid)
	}
	p.mu.Lock()
	for _, mp := range p.processes {
		// Prefer the PID captured at Start (survives Process.Release on
		// the detach path); fall back to the live handle for any process
		// registered without a captured PID. Skip never-started / already-
		// released-without-capture entries so we never persist a 0/-1 that
		// `forge env down` would try to signal.
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
	// truncated/corrupt ledger that `forge env down` would misparse. The temp
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

// resolvedEnvState is the launch-time discovery snapshot persisted next to
// the PID ledger (upEnvStatePath). It records the facts a `forge env status`
// re-render cannot recover on its own: the LIVE name→port map (ephemeral
// bind-:0 ports are assigned in the up process and reverted by the status
// render's resolve_port restore) and the resolved DATABASE_URL. The JSON tags
// are an on-disk cache format, not a public contract — additive changes only.
type resolvedEnvState struct {
	Env         string         `json:"env"`
	DatabaseURL string         `json:"database_url,omitempty"`
	Ports       map[string]int `json:"ports"` // service/frontend name → live listen port
	UpdatedAt   string         `json:"updated_at"`
}

// persistResolvedEnv writes the sibling <env>.env.json discovery cache from
// the resolved entities `forge env up` just launched: the name→port map (from
// the same collectUpServices rows the summary printed) and the DATABASE_URL
// (via resolveSeedDSN, the same source auto-seed uses). Best-effort — a write
// failure only costs `forge env status` its live-port overlay, never the run.
func persistResolvedEnv(projectID, env string, entities *KCLEntities, cfg *config.ProjectConfig, targets []string, frontendsOn bool) {
	path, err := upEnvStatePath(projectID, env)
	if err != nil {
		fmt.Printf("[up] warning: resolve env state path: %v\n", err)
		return
	}
	// nil probe: we only want the resolved ports, not a liveness check.
	rows := collectUpServices(entities, env, targets, frontendsOn, nil)
	ports := make(map[string]int, len(rows))
	for _, r := range rows {
		if r.Port > 0 {
			ports[r.Name] = r.Port
		}
	}
	st := resolvedEnvState{
		Env:         env,
		DatabaseURL: resolveSeedDSN(entities, cfg, env),
		Ports:       ports,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		fmt.Printf("[up] warning: encode env state: %v\n", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Printf("[up] warning: mkdir env state: %v\n", err)
		return
	}
	// Atomic temp+rename so a status read never sees a half-written file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		fmt.Printf("[up] warning: write env state: %v\n", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Printf("[up] warning: commit env state: %v\n", err)
		_ = os.Remove(tmp)
	}
}

// loadResolvedEnv reads the sibling <env>.env.json discovery cache. Returns
// nil (never an error) when absent or unparseable so callers degrade to the
// re-rendered ports rather than failing a read-only status command.
func loadResolvedEnv(projectID, env string) *resolvedEnvState {
	path, err := upEnvStatePath(projectID, env)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var st resolvedEnvState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil
	}
	return &st
}

// overlayResolvedPorts replaces each row's port/URL with the LIVE port the
// launch persisted (keyed by name), so a status snapshot reports the actual
// ephemeral port instead of the drift-reverted render value. Rows with no
// persisted port keep whatever the render produced.
func overlayResolvedPorts(rows []upServiceRow, ports map[string]int) {
	for i := range rows {
		if p, ok := ports[rows[i].Name]; ok && p > 0 {
			rows[i].Port = p
			rows[i].URL = fmt.Sprintf("http://localhost:%d", p)
		}
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

	removeStackRecords(p.projectID, p.env)
}

// trackedProc is one (name, pid) entry parsed from a stack's PID ledger.
type trackedProc struct {
	name string
	pid  int
}

// trackedStack parses a stack's ledger (<project-id>/<env>.pids) into its
// (name, pid) entries. Returns nil when no ledger is present.
//
// It is a RECORD of what this project's `forge env up` started, and teardown's
// FALLBACK — not its authority. See stackTeardownRoots: a remembered pid is
// signalled only when the live markers cannot answer at all, because the
// process wearing that pid today may not be the one forge started.
func trackedStack(projectID, env string) []trackedProc {
	statePath, err := upStatePath(projectID, env)
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
		if _, err := fmt.Sscanf(parts[1], "%d", &pid); err != nil || pid <= 0 {
			continue
		}
		out = append(out, trackedProc{name: parts[0], pid: pid})
	}
	return out
}

// runUpStop stops the stack this project is running for env.
//
// The project is resolved STRICTLY: without a forge.yaml above the working
// directory there is no project id, and therefore no way to know whose stack to
// stop. That must be an error. It used to fall back to hashing "." — which
// matches no recorded stack — so `forge env down dev` from the wrong directory
// printed "no tracked stack for env=dev" and exited 0 while the stack it meant
// to stop kept running. A teardown that cannot fail is not a teardown.
func runUpStop(env string) error {
	projectDir, err := requireProjectDir("forge env down")
	if err != nil {
		return err
	}
	projectID := projectIDForDir(projectDir)
	stopped := stopStack(projectID, env)

	// Host infrastructure is stopped SEPARATELY, because it is not one of
	// the child processes the ledger tracks. A host-run postgres is started
	// through `pg_ctl start`, which daemonizes: the process forge spawned
	// exits immediately and the server it left behind is not forge's child,
	// carries none of forge's ownership markers, and outlives the `forge
	// run` that started it — by design, so a dev stack keeps serving after
	// the command returns. Signalling it the way stopStack signals a child
	// would be both impossible (no marker to find it by) and wrong (SIGKILL
	// leaks the SysV IPC an orderly shutdown releases).
	//
	// Its DATA survives; this stops the server, it does not reset the
	// database. Best-effort: a failure here is reported but must not stop
	// the rest of the teardown.
	infraStopped, err := stopHostInfra(env)
	if err != nil {
		fmt.Printf("[up] host infra: %v\n", err)
	}

	if stopped == 0 && infraStopped == 0 {
		fmt.Printf("[up] no forge processes running for env=%s in %s.\n", env, projectDir)
		return nil
	}
	if stopped > 0 {
		fmt.Printf("[up] stopped %d process tree(s) for env=%s.\n", stopped, env)
	}
	if infraStopped > 0 {
		fmt.Printf("[up] stopped %d host infrastructure server(s) for env=%s (data preserved).\n", infraStopped, env)
	}
	return nil
}

// stopHostInfra shuts down every `forge.HostInfra` server the env declares
// and reports how many were actually running.
//
// The env's KCL is re-rendered to learn what to stop, which is the same
// source `up` read to start it — so an instance is found by its
// DECLARATION rather than by scanning for stray postgres processes. That
// matters on a shared machine: forge stops the server whose data directory
// this project declares, and cannot mistake a colleague's (or another
// project's) database for its own.
//
// A render failure is reported, not fatal: `forge env down` must still stop
// the host processes it can reach even when the KCL no longer renders.
func stopHostInfra(env string) (int, error) {
	projectDir := projectDirForKCL()
	entities, err := RenderKCL(context.Background(), projectDir, env)
	if err != nil {
		return 0, fmt.Errorf("render KCL to find declared host infrastructure: %w", err)
	}
	var failures []error
	count := 0
	for _, svc := range entities.Services {
		if svc.Deploy.Type != "host-infra" || svc.Deploy.HostInfra == nil {
			continue
		}
		hi := svc.Deploy.HostInfra
		spec := hostinfra.Spec{
			Name:            svc.Name,
			Engine:          hi.Engine,
			Port:            hi.Port,
			Database:        hi.Database,
			User:            hi.User,
			Password:        hi.Password,
			DataDir:         hi.DataDir,
			Version:         hi.Version,
			IDPDatabase:     hi.IDPDatabase,
			IDPDatabasePort: hi.IDPDatabasePort,
			IDPMasterKey:    hi.IDPMasterKey,
			IDPStepsFile:    hi.IDPStepsFile,
			IDPPATPath:      hi.IDPPATPath,
		}
		stopped, serr := hostinfra.StopReport(projectDir, spec)
		if serr != nil {
			failures = append(failures, serr)
			continue
		}
		if stopped {
			count++
		}
	}
	return count, errors.Join(failures...)
}

// upLogPath returns the well-known log file for a host service or
// frontend started by `forge env up`. Logs land under the project-relative
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
