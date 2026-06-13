package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/devproxy"
	"github.com/reliant-labs/forge/internal/hostlaunch"
)

// ANSI color codes for service log prefixes.
var serviceColors = []string{
	"\033[36m", // cyan
	"\033[33m", // yellow
	"\033[35m", // magenta
	"\033[32m", // green
	"\033[34m", // blue
	"\033[31m", // red
	"\033[96m", // bright cyan
	"\033[93m", // bright yellow
}

const colorReset = "\033[0m"

// managedProcess tracks a running child process.
type managedProcess struct {
	name string
	cmd  *exec.Cmd
	// ports are the child's declared listen ports (fe.Port for
	// frontends, the per-service ports for the single server binary).
	// Used by the port-conflict diagnosis when the child dies
	// unexpectedly. May be empty when no port is known.
	ports []int
	// done is closed by superviseChild once cmd.Wait has returned.
	// exitErr holds Wait's result; it is written before done closes
	// and is safe to read after done is closed (or after receiving
	// this process from the exit-notification channel).
	done    chan struct{}
	exitErr error
}

// superviseChild owns the single allowed cmd.Wait call for a managed
// child. It waits for the output streams to hit EOF first — os/exec
// documents that Wait closes StdoutPipe/StderrPipe, so calling it with
// unread data still buffered truncates the child's final output lines,
// which for a dying child are exactly the lines we need. It then
// records the exit on the managedProcess, closes done, and notifies
// exitCh. The notification is non-blocking: the main loop only acts on
// the first death, and during shutdown nobody is listening.
func superviseChild(p *managedProcess, streams *sync.WaitGroup, exitCh chan<- *managedProcess) {
	go func() {
		if streams != nil {
			streams.Wait()
		}
		p.exitErr = p.cmd.Wait()
		close(p.done)
		select {
		case exitCh <- p:
		default:
		}
	}()
}

// defaultRunEnvironment decides whether runProjectDev should inject
// ENVIRONMENT=development into the server child's environment.
//
// The scaffolded config.dev.yaml can be effectively empty; without an
// ENVIRONMENT value the binary defaults to production, where the auth
// interceptor refuses to start ("no auth provider configured") — so the
// canonical dev command would boot children in production mode. The
// default fires only when ENVIRONMENT is declared NOWHERE: not in the
// per-env config projection (envConfigToEnvVars maps the per-env config
// key "environment" → "ENVIRONMENT") and not in the shell (lookupEnv is
// os.LookupEnv in production code; a present-but-empty value still
// counts as explicit). Explicit env always wins — hostlaunch.MergeEnv
// already gives os.Environ() precedence over the extras map, so even if
// both raced, the shell value would land in the child.
//
// The default also applies only to `--env dev` (the flag default).
// Running `forge run --env staging` names a non-dev environment
// explicitly; silently flipping it to development would be a lie —
// there the per-env config alone decides.
func defaultRunEnvironment(envExtraEnv map[string]string, lookupEnv func(string) (string, bool), env string) (string, bool) {
	if env != "dev" {
		return "", false
	}
	if _, declared := envExtraEnv["ENVIRONMENT"]; declared {
		return "", false
	}
	if _, declared := lookupEnv("ENVIRONMENT"); declared {
		return "", false
	}
	return "development", true
}

// composeDevCORSOrigins builds the dev default for CORS_ORIGINS: the
// browser-visible origins `forge run` is about to create. Each
// frontend's loopback origins (http://localhost:<fe.Port> AND
// http://127.0.0.1:<fe.Port>) plus — unless the proxy is disabled —
// the dev-proxy hostnames (http://<name>.localhost:<proxyPort>,
// http://localhost:<proxyPort>, http://127.0.0.1:<proxyPort>).
//
// Both loopback spellings are mandatory: the browser sends whatever
// the user typed as the Origin header, and a 127.0.0.1 URL with a
// localhost-only allowlist renders the UI but CORS-blocks every fetch
// (journey fr-5b2121e48f). `<name>.localhost` needs no 127.0.0.1 twin
// — it's a hostname, not a spelling of one.
//
// Comma-separated to match the generated config loader, which splits
// cors_origins/CORS_ORIGINS on "," (config.go.tmpl / pkg/middleware
// CORS). Returns "" when no frontend declares a port — backend-only
// projects have no browser origin to allow, and an empty return tells
// the caller to inject nothing.
func composeDevCORSOrigins(frontends []config.FrontendConfig, proxyPort int, noProxy bool) string {
	var out []string
	seen := map[string]struct{}{}
	add := func(origin string) {
		if _, dup := seen[origin]; dup {
			return
		}
		seen[origin] = struct{}{}
		out = append(out, origin)
	}
	for _, fe := range frontends {
		if fe.Port <= 0 {
			continue
		}
		add(fmt.Sprintf("http://localhost:%d", fe.Port))
		add(fmt.Sprintf("http://127.0.0.1:%d", fe.Port))
	}
	if len(out) == 0 {
		return ""
	}
	if !noProxy && proxyPort > 0 {
		for _, fe := range frontends {
			if fe.Port <= 0 {
				continue
			}
			add(fmt.Sprintf("http://%s.localhost:%d", fe.Name, proxyPort))
		}
		add(fmt.Sprintf("http://localhost:%d", proxyPort))
		add(fmt.Sprintf("http://127.0.0.1:%d", proxyPort))
	}
	return strings.Join(out, ",")
}

// frontendDevEnv composes the child environment for a frontend dev
// server. forge.yaml is the source of truth for dev/prod parity, so the
// declared values are FORCE-injected (withForcedEnv replaces any stale
// PORT / NEXT_PUBLIC_BASE_PATH that bled in from the parent shell):
//
//   - PORT: the forge.yaml frontend port, so the dev proxy can dispatch
//     to a deterministic loopback port.
//   - NEXT_PUBLIC_BASE_PATH: the forge.yaml base_path, so a frontend
//     mounted under a prefix in production serves the same prefix in
//     dev (next.config reads it; without this the setting is dead in
//     `forge run`).
func frontendDevEnv(base []string, fe config.FrontendConfig) []string {
	env := base
	if fe.Port > 0 {
		env = withForcedEnv(env, "PORT", strconv.Itoa(fe.Port))
	}
	if fe.BasePath != "" {
		env = withForcedEnv(env, "NEXT_PUBLIC_BASE_PATH", fe.BasePath)
	}
	return env
}

// diagnosePortConflict checks whether the given port is currently
// bound by trying a quick loopback listen. Returns a human hint when
// the port is held (the most common reason a dev child dies instantly
// — a stale dev server from a previous session) and "" when the port
// is free or unknown (<= 0). Best-effort: a successful probe listener
// is closed immediately.
func diagnosePortConflict(port int) string {
	if port <= 0 {
		return ""
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		return fmt.Sprintf("port %d is already in use — another process (a stale dev server?) holds it", port)
	}
	_ = ln.Close()
	return ""
}

// describeChildExit renders the loud failure line for a child that died
// before shutdown was requested.
func describeChildExit(name string, exitErr error) string {
	if exitErr == nil {
		return fmt.Sprintf("process %q exited unexpectedly (exit status 0)", name)
	}
	return fmt.Sprintf("process %q exited unexpectedly: %v", name, exitErr)
}

// childExitError is the non-nil error runProjectDev returns when a
// child dies before shutdown — `forge run` must exit nonzero even for
// a status-0 child: a dev server that stopped serving is a failure
// regardless of how politely it left.
func childExitError(name string, exitErr error) error {
	if exitErr == nil {
		return fmt.Errorf("process %q exited unexpectedly (exit status 0)", name)
	}
	return fmt.Errorf("process %q exited unexpectedly: %w", name, exitErr)
}

// runOptions holds flags for the run command.
type runOptions struct {
	env      string
	noInfra  bool
	services []string
	debug    bool
	// proxyPort is the localhost port the cross-frontend dev proxy
	// binds to. 0 (default) uses [defaultDevProxyPort] (8080), unless
	// FORGE_RUN_PROXY_PORT is set in the env. -1 disables the proxy
	// (set via --no-proxy) for the rare case the user wants the raw
	// per-frontend ports without a single unified URL.
	proxyPort int
	noProxy   bool
}

// defaultDevProxyPort is the bind port for the cross-frontend dev
// proxy when neither --proxy-port nor FORGE_RUN_PROXY_PORT is set.
//
// CAUTION: 8080 is ALSO the first scaffolded service's default port
// (`forge add service` auto-increments from 8080), so this constant is
// only the starting point — resolveProxyPort skips past every declared
// component port before settling. Sharing the number was the
// split-brain of journey fr-5b2121e48f: the proxy got 127.0.0.1:8080,
// the service wildcard-bound the same port, and the browser's
// localhost raced between them by address family.
const defaultDevProxyPort = 8080

// runProjectDev orchestrates the local development environment.
func runProjectDev(opts runOptions) error {
	store, err := loadProjectStore()
	if err != nil {
		return err
	}
	cfg := store.Config()

	fmt.Printf("[run] Starting project: %s (env: %s)\n", store.Meta().Name, opts.env)
	if opts.noInfra {
		fmt.Println("[run] Skipping infrastructure (--no-infra)")
	}
	if len(opts.services) > 0 {
		fmt.Printf("[run] Running only: %v\n", opts.services)
	}

	// Resolve per-env config from the config.<env>.yaml sibling file.
	// Missing file or empty config is non-fatal — we log it and continue
	// with whatever the binary's startup defaults provide.
	projectDir, perr := findProjectConfigFile()
	envExtraEnv := map[string]string{}
	if perr == nil {
		dir := filepath.Dir(projectDir)
		envCfg, lerr := config.LoadEnvironmentConfig(dir, opts.env)
		if lerr != nil {
			fmt.Printf("[run] No per-env config for %q (%v); using binary defaults.\n", opts.env, lerr)
		} else {
			envExtraEnv = envConfigToEnvVars(envCfg, projectDir)
			if len(envExtraEnv) > 0 {
				fmt.Printf("[run] Loaded %d per-env config values from environment %q.\n", len(envExtraEnv), opts.env)
			}
		}
	}

	// Default the children to dev mode. See defaultRunEnvironment for
	// the full rationale: an effectively-empty config.dev.yaml leaves
	// ENVIRONMENT unset, the binary defaults to production, and the
	// auth interceptor aborts startup — the canonical dev command must
	// not boot children in production mode. Fires only for the default
	// env ("dev") and only when ENVIRONMENT is set nowhere else.
	if v, ok := defaultRunEnvironment(envExtraEnv, os.LookupEnv, opts.env); ok {
		envExtraEnv["ENVIRONMENT"] = v
		fmt.Println("[run] ENVIRONMENT=development (forge run defaults children to dev mode; set ENVIRONMENT to override)")

		// Auth bypass is NEVER implied by ENVIRONMENT — a project with a
		// real auth provider keeps auth enforced under `forge run`. The
		// ONLY auto-bypass is for a project with no auth provider at all,
		// which would otherwise refuse to boot (the interceptor needs a
		// validator). There's nothing to bypass, so enable dev claims so
		// the dev loop works — explicitly (AUTH_DEV_MODE) and loudly.
		_, authModeInCfg := envExtraEnv["AUTH_DEV_MODE"]
		_, authModeInShell := os.LookupEnv("AUTH_DEV_MODE")
		if !authModeInCfg && !authModeInShell {
			provider := strings.ToLower(strings.TrimSpace(cfg.Auth.Provider))
			if provider == "" || provider == "none" {
				envExtraEnv["AUTH_DEV_MODE"] = "true"
				fmt.Println("[run] AUTH_DEV_MODE=true (no auth provider configured → auth bypassed for dev, dev claims injected). Configure an auth provider, or set AUTH_DEV_MODE=false, to enforce auth.")
			}
		}
	}
	fmt.Println()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Catch SIGINT/SIGTERM for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var (
		mu        sync.Mutex
		outputMu  sync.Mutex
		processes []*managedProcess
		colorIdx  int
	)

	nextColor := func() string {
		c := serviceColors[colorIdx%len(serviceColors)]
		colorIdx++
		return c
	}

	// childExitCh carries exit notifications from superviseChild's
	// per-child wait goroutines back to the main loop, so a child that
	// dies while `forge run` is supposedly healthy is surfaced loudly
	// instead of discovered (or not) at shutdown. Buffered + non-blocking
	// sends: only the first death matters, the rest are reported via the
	// shutdown path.
	childExitCh := make(chan *managedProcess, 8)

	// startProcess starts a command and registers it for cleanup. ports
	// are the child's declared listen ports (zero values are dropped),
	// used for the port-conflict diagnosis if the child dies.
	startProcess := func(name string, cmd *exec.Cmd, ports ...int) error {
		color := nextColor()
		prefix := fmt.Sprintf("%s[%s]%s ", color, name, colorReset)

		// Pipe stdout/stderr with colored prefixes.
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("failed to create stdout pipe for %s: %w", name, err)
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return fmt.Errorf("failed to create stderr pipe for %s: %w", name, err)
		}

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("failed to start %s: %w", name, err)
		}

		p := &managedProcess{name: name, cmd: cmd, done: make(chan struct{})}
		for _, port := range ports {
			if port > 0 {
				p.ports = append(p.ports, port)
			}
		}
		mu.Lock()
		processes = append(processes, p)
		mu.Unlock()

		// Stream output in background goroutines; the supervisor waits
		// for both streams to drain before reaping the child (see
		// superviseChild for the Wait-vs-pipes ordering constraint).
		var streams sync.WaitGroup
		streams.Add(2)
		go func() { defer streams.Done(); streamWithPrefix(prefix, stdout, &outputMu) }()
		go func() { defer streams.Done(); streamWithPrefix(prefix, stderr, &outputMu) }()
		superviseChild(p, &streams, childExitCh)

		return nil
	}

	// Registration view (pkg/app/services.go): what the binary serves
	// is the user-owned row list, not forge.yaml. Best-effort — a
	// missing/broken registry falls open to "everything registered";
	// the generated BootstrapOnly guard is the runtime backstop.
	reg := &serviceRegistry{Exists: false}
	if perr == nil {
		if r, regErr := loadServiceRegistry(filepath.Dir(projectDir)); regErr == nil {
			reg = r
		}
	}
	notRegistered := func(s config.ComponentConfig) bool {
		return isConnectServiceConfig(s) && !reg.registered(s.Name)
	}

	// Filter servers/frontends based on --service flag. Only server
	// components have a listen port and a Connect surface to run in the
	// dev loop; workers/crons/operators run in-process under the server
	// binary, and binaries are separate subcommands.
	servicesToRun := cfg.Servers()
	frontendsToRun := cfg.Frontends
	if len(opts.services) > 0 {
		servicesToRun = filterServicesByNames(cfg.Servers(), opts.services)
		frontendsToRun = filterFrontendsByNames(cfg.Frontends, opts.services)
		if len(servicesToRun) == 0 && len(frontendsToRun) == 0 {
			return fmt.Errorf("none of the specified services %v found in project config", opts.services)
		}
		// Explicitly requesting an unregistered service is a
		// misconfiguration — fail here with the full story instead of
		// letting the generated BootstrapOnly guard error later.
		for _, s := range servicesToRun {
			if notRegistered(s) {
				return fmt.Errorf("service %q is not registered in %s — this binary does not serve it; add its serviceRow line to RegisteredServices to serve it here", s.Name, serviceRegistryRelPath)
			}
		}
	} else {
		// Default run: unregistered services have no row in the appkit
		// table, so passing their names to the server would trip the
		// generated BootstrapOnly guard. Skip them with a notice.
		var registered []config.ComponentConfig
		for _, s := range servicesToRun {
			if !notRegistered(s) {
				registered = append(registered, s)
				continue
			}
			fmt.Printf("[run] skipping %s (no serviceRow in %s — not served by this binary)\n", s.Name, serviceRegistryRelPath)
		}
		servicesToRun = registered
	}

	// Resolve the dev proxy port BEFORE anything starts: the proxy must
	// never share a port with a component it fronts (the first
	// scaffolded service defaults to 8080 — the same number as
	// defaultDevProxyPort), and an explicit flag/env collision should
	// abort before docker compose spins up. Resolved once and reused by
	// the CORS dev default and the proxy startup below.
	proxyPort := 0
	if !opts.noProxy {
		declared := declaredComponentPorts(servicesToRun, frontendsToRun)
		pp, perr := resolveProxyPort(opts.proxyPort, declared)
		if perr != nil {
			return perr
		}
		proxyPort = pp
		if owner, shifted := declared[defaultDevProxyPort]; shifted && pp != defaultDevProxyPort && opts.proxyPort <= 0 {
			fmt.Printf("[run] Dev proxy: default port %d is %s's port; using %d instead.\n", defaultDevProxyPort, owner, pp)
		}
	}

	// 1. Start infrastructure via docker compose (unless --no-infra).
	if !opts.noInfra {
		composePath := "docker-compose.yml"
		if _, err := os.Stat(composePath); err == nil {
			// Preflight the postgres publish port: a host postgres on
			// 5432 otherwise dies inside compose with an unactionable
			// "exit status 1". The error names the exact
			// POSTGRES_PORT=<free> rerun.
			if pgPort, perr := strconv.Atoi(envOrDefault("POSTGRES_PORT", "5432")); perr == nil {
				if err := preflightPostgresPort(pgPort, func() bool { return composeHasRunningPostgres(ctx, composePath) }); err != nil {
					return err
				}
			}
			fmt.Println("[run] Starting infrastructure...")
			composeCmd := exec.CommandContext(ctx, "docker", "compose",
				"-f", composePath, "up", "-d",
			)
			composeCmd.Stdout = os.Stdout
			composeCmd.Stderr = os.Stderr
			if err := composeCmd.Run(); err != nil {
				return fmt.Errorf("failed to start infrastructure: %w", err)
			}
			fmt.Println()

			// Wire the host-side dev loop to the compose postgres with
			// zero hand-editing: discover the published host port and
			// inject DATABASE_URL into the child env when the developer
			// hasn't set it anywhere themselves. The compose template
			// publishes a fixed loopback port (POSTGRES_PORT, default
			// 5432), but discovery keeps this correct even when the user
			// overrides the mapping. Shell env and per-env config always
			// win — this only fills the empty default.
			if os.Getenv("DATABASE_URL") == "" {
				if _, declared := envExtraEnv["DATABASE_URL"]; !declared {
					if url := discoverComposeDatabaseURL(ctx, composePath, cfg.Name); url != "" {
						envExtraEnv["DATABASE_URL"] = url
						fmt.Printf("[run] DATABASE_URL → %s (discovered from docker compose)\n", redactDSNPassword(url))
					}
				}
			}
		}
	}

	// Dev CORS default: the scaffolded frontend transport calls the API
	// cross-origin while the server's cors_origins config defaults
	// empty, so browser CRUD fails CORS preflight out of the box. When
	// the user set CORS_ORIGINS nowhere (shell or per-env config) and
	// we're launching at least one frontend, allow the origins this
	// very command is about to create. composeDevCORSOrigins documents
	// the list shape; comma-separated to match the generated config
	// loader.
	if _, declared := envExtraEnv["CORS_ORIGINS"]; !declared {
		if _, inShell := os.LookupEnv("CORS_ORIGINS"); !inShell {
			if origins := composeDevCORSOrigins(frontendsToRun, proxyPort, opts.noProxy); origins != "" {
				envExtraEnv["CORS_ORIGINS"] = origins
				fmt.Printf("[run] CORS_ORIGINS → %s (dev default so the browser can reach the API; set cors_origins to override)\n", origins)
			}
		}
	}

	// 2. Start Go binary via Air (hot reload) or go run fallback.
	// Single binary architecture: one process with service names as args.
	//
	// The start logic lives in a closure because the wait loop may call
	// it a second time: runWaitLoop grants the server child ONE
	// automatic restart. Journey fr-00ff2c98d2: a generate-induced
	// double rebuild raced air into 'bind: address already in use', air
	// exited Code 1, and the orchestrator silently kept serving the
	// proxy + frontend over a dead backend (healthz=000, no message).
	var startServerProcess func() error
	serverName := ""
	if len(servicesToRun) > 0 || len(opts.services) == 0 {
		var serviceNames []string
		for _, svc := range servicesToRun {
			serviceNames = append(serviceNames, svc.Name)
		}

		airConfig := ".air.toml"
		if opts.debug {
			debugConfig := ".air-debug.toml"
			if _, err := os.Stat(debugConfig); err == nil {
				airConfig = debugConfig
			}
		}

		// Layer per-env config (forge.yaml/sibling) onto the subprocess
		// environment so the binary's flag/env loader sees the values.
		// Existing process env wins (a developer can still override
		// inline) — we apply the per-env values first, then anything
		// already set in os.Environ().
		baseEnv := hostlaunch.MergeEnv(envExtraEnv, os.Environ())
		if opts.debug {
			// --debug always forces development (Delve against a
			// production-mode binary is never the intent). exec.Cmd
			// keeps the LAST duplicate, so this appended entry beats
			// both the per-env config and the shell.
			baseEnv = append(baseEnv, "ENVIRONMENT=development")
		}
		// Declared service ports feed the port-conflict diagnosis when
		// the server child dies (the single binary binds one listener
		// per registered service).
		var serverPorts []int
		for _, svc := range servicesToRun {
			serverPorts = append(serverPorts, svc.PrimaryPort())
		}

		serverName = cfg.Name
		startServerProcess = func() error {
			var cmd *exec.Cmd
			if opts.debug {
				// Try air with debug config first
				if _, err := os.Stat(airConfig); err == nil {
					cmd = exec.CommandContext(ctx, "air", "-c", airConfig)
				} else {
					// Fallback: build with debug flags and run under Delve
					fmt.Println("[run] No .air-debug.toml found, building debug binary and starting Delve...")
					if err := os.MkdirAll(".forge/debug", 0o755); err != nil {
						return fmt.Errorf("failed to create debug output directory: %w", err)
					}
					buildCmd := exec.CommandContext(ctx, "go", "build", "-gcflags=all=-N -l", "-o", ".forge/debug/"+cfg.Name, "./cmd")
					buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
					buildCmd.Stdout = os.Stdout
					buildCmd.Stderr = os.Stderr
					if err := buildCmd.Run(); err != nil {
						return fmt.Errorf("failed to build debug binary: %w", err)
					}
					absBin, _ := filepath.Abs(".forge/debug/" + cfg.Name)
					dlvArgs := []string{"exec", "--headless", "--listen=:2345", "--api-version=2", "--accept-multiclient", "--continue", absBin, "--", "server"}
					dlvArgs = append(dlvArgs, serviceNames...)
					cmd = exec.CommandContext(ctx, "dlv", dlvArgs...)
				}
				fmt.Println("[run] Delve debugger listening on :2345 \u2014 attach with VS Code or 'dlv connect :2345'")
			} else if cfg.EffectiveHotReload() {
				if _, err := os.Stat(airConfig); err == nil {
					cmd = exec.CommandContext(ctx, "air", "-c", airConfig)
				} else {
					// HotReload enabled but no air config found; fall back to go run
					args := []string{"run", "./cmd", "server"}
					args = append(args, serviceNames...)
					cmd = exec.CommandContext(ctx, "go", args...)
				}
			} else {
				// HotReload disabled: run go directly without air
				args := []string{"run", "./cmd", "server"}
				args = append(args, serviceNames...)
				cmd = exec.CommandContext(ctx, "go", args...)
			}
			// Always assign. This was previously gated on len(envExtraEnv)>0
			// (or --debug), so an empty per-env config left cmd.Env nil and
			// none of the injected defaults (ENVIRONMENT, DATABASE_URL,
			// CORS_ORIGINS) could ever reach the child. With nothing to
			// inject, baseEnv == os.Environ(), so this is also a no-op for
			// the legacy path.
			cmd.Env = baseEnv
			cmd.Dir = "."
			return startProcess(cfg.Name, cmd, serverPorts...)
		}
		// A server that cannot start at all is a dead dev loop — fail
		// loudly here instead of warning and serving frontends over
		// nothing (the same honesty rule as the mid-session death).
		if err := startServerProcess(); err != nil {
			return fmt.Errorf("failed to start the server process: %w", err)
		}
	}

	// 3. Start Next.js frontends. PORT and NEXT_PUBLIC_BASE_PATH are
	// force-injected from the forge.yaml declaration (the source of
	// truth for dev/prod parity) so the proxy can dispatch to a
	// deterministic loopback port and a declared base_path is live in
	// dev — even if stale values bled in from the parent shell. See
	// frontendDevEnv; mirrors the `forge up` host-mode shape
	// (buildFrontendCmd in up.go).
	for _, fe := range frontendsToRun {
		cmd := exec.CommandContext(ctx, "npm", "run", "dev")
		cmd.Dir = fe.Path
		cmd.Env = frontendDevEnv(os.Environ(), fe)
		if err := startProcess(fe.Name, cmd, fe.Port); err != nil {
			fmt.Printf("[run] Warning: %v\n", err)
		}
	}

	if len(processes) == 0 {
		fmt.Println("[run] No services or frontends to run.")
		return nil
	}

	// 4. Cross-frontend dev proxy. Maps `<name>.localhost:<proxy>` →
	// the per-component loopback port so every frontend + HTTP service
	// is reachable under one URL with prod-parity hostnames. KCL
	// HTTPRoute entities are the source of truth for which host maps
	// to which backend; frontends contribute a default
	// `<name>.localhost` entry for any frontend not referenced by an
	// HTTPRoute (the common dev case, where users haven't yet
	// declared ingress).
	//
	// The listeners are bound SYNCHRONOUSLY (devproxy.ListenLoopback,
	// one per loopback family — see that function for why a single
	// `localhost:` bind was the fr-5b2121e48f split-brain), so a bind
	// failure aborts here, loudly, before "N process(es) started" is
	// ever printed. devGoroutineErrCh only carries the rare post-bind
	// Serve failures (OS revoked the socket — EMFILE or similar);
	// buffered + non-blocking sends, first error wins.
	//
	// runErr, when non-nil, aborts the dev session: children are still
	// stopped gracefully below, but runProjectDev returns it so the
	// forge process exits nonzero.
	var runErr error
	devGoroutineErrCh := make(chan error, 4)
	if !opts.noProxy {
		routes := loadDevProxyRoutes(ctx, opts.env)
		backends := buildDevProxyBackends(frontendsToRun, servicesToRun, routes)
		if len(backends) == 0 {
			fmt.Println("[run] Dev proxy: no frontends or HTTP-routed services declared; skipping.")
		} else {
			router := devproxy.New(backends)
			srv := &http.Server{Handler: router, ReadHeaderTimeout: 10 * time.Second}
			listeners, lerr := devproxy.ListenLoopback(proxyPort)
			if lerr != nil {
				fmt.Fprintf(os.Stderr, "\n[run] FATAL: dev proxy: %v\n", lerr)
				fmt.Fprintf(os.Stderr, "[run] is a stale dev server holding port %d? use --proxy-port to move the proxy\n", proxyPort)
				runErr = fmt.Errorf("dev startup aborted: %w", lerr)
			} else {
				printDevProxyBanner(proxyPort, backends)
				for _, ln := range listeners {
					go func(ln net.Listener) {
						if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
							// Non-blocking: if the channel is full, the first
							// error already won — drop ours rather than block
							// the goroutine on a reader that's no longer
							// waiting.
							select {
							case devGoroutineErrCh <- fmt.Errorf("dev proxy listener on %s: %w", ln.Addr(), err):
							default:
							}
						}
					}(ln)
				}
				// Hook proxy shutdown into the global ctx cancel so it
				// tears down alongside the child processes on Ctrl-C.
				go func() {
					<-ctx.Done()
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = srv.Shutdown(shutdownCtx)
				}()
			}
		}
	}

	// announceChildDeath is the unmissable banner for a child that died
	// before shutdown was requested: it names the process and exit
	// status, plus a best-effort port diagnosis (EADDRINUSE from a
	// stale dev server is the overwhelmingly common cause of an
	// instant death). Printed BEFORE any restart/abort decision so the
	// death is loud either way.
	announceChildDeath := func(p *managedProcess) {
		fmt.Fprintf(os.Stderr, "\n[run] ==================================================================\n")
		fmt.Fprintf(os.Stderr, "[run] FATAL: %s\n", describeChildExit(p.name, p.exitErr))
		for _, port := range p.ports {
			if msg := diagnosePortConflict(port); msg != "" {
				fmt.Fprintf(os.Stderr, "[run] %s\n", msg)
			}
		}
		fmt.Fprintf(os.Stderr, "[run] ==================================================================\n")
	}
	// reportChildExit: the banner plus a non-nil error so `forge run`
	// exits nonzero instead of reporting "N process(es) started" over a
	// corpse.
	reportChildExit := func(p *managedProcess) error {
		announceChildDeath(p)
		return childExitError(p.name, p.exitErr)
	}

	if runErr == nil {
		fmt.Printf("\n[run] %d process(es) started. Press Ctrl+C to stop.\n\n", len(processes))

		// Startup race: a child that can't bind its port dies within a
		// few hundred microseconds of Start. Give it a short window to
		// surface before we hand over to the wait loop — an instant
		// death is a configuration problem (stale dev server on the
		// port), not the transient rebuild race the wait loop's restart
		// exists for, so it aborts without a retry.
		select {
		case err := <-devGoroutineErrCh:
			fmt.Fprintf(os.Stderr, "[run] FATAL: %v\n", err)
			runErr = fmt.Errorf("dev startup aborted: %w", err)
		case p := <-childExitCh:
			runErr = reportChildExit(p)
		case <-time.After(250 * time.Millisecond):
			// Every child survived its first moments (or a failure is
			// still in flight and the wait loop picks it up).
		}
	}

	if runErr == nil {
		runErr = runWaitLoop(sigCh, devGoroutineErrCh, childExitCh, serverName, startServerProcess, announceChildDeath)
	}
	cancel()

	// Gracefully stop all child processes.
	mu.Lock()
	toStop := make([]*managedProcess, len(processes))
	copy(toStop, processes)
	mu.Unlock()

	for _, p := range toStop {
		if p.cmd.Process != nil {
			fmt.Printf("[run]   Stopping %s (pid %d)...\n", p.name, p.cmd.Process.Pid)
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
		}
	}

	// Wait for processes to exit with a single global timeout (10s for
	// the whole set, not per-process). Each child's exit is observed by
	// its superviseChild goroutine — the one allowed cmd.Wait call —
	// so shutdown waits on the done channels instead of calling Wait a
	// second time (a second Wait on the same cmd errors).
	allDone := make(chan struct{})
	go func() {
		for _, p := range toStop {
			<-p.done
		}
		close(allDone)
	}()

	select {
	case <-allDone:
		// Every process exited cleanly within the shared budget.
	case <-time.After(10 * time.Second):
		// Single global timeout reached — SIGKILL anything still running
		// in one pass, then wait for the forced exits to flush.
		for _, p := range toStop {
			select {
			case <-p.done:
				// Already exited.
			default:
				if p.cmd.Process != nil {
					fmt.Printf("[run]   %s (pid %d) did not exit after SIGTERM, killing...\n", p.name, p.cmd.Process.Pid)
					_ = p.cmd.Process.Kill()
				}
			}
		}
		<-allDone
	}

	if !opts.noInfra {
		teardownInfrastructure()
	}

	fmt.Println("[run] All processes stopped.")
	return runErr
}

// runWaitLoop is the post-startup supervisor for `forge run`: it
// blocks until a signal (clean shutdown, nil return), a dev-proxy
// goroutine error, or a child death ends the session.
//
// The honesty contract (journey fr-00ff2c98d2: air exited Code 1 after
// a generate-induced rebuild race and the orchestrator kept serving
// the proxy + frontend over a dead backend with healthz=000 and no
// message):
//
//   - every child death is announced via announceExit BEFORE any
//     decision, so the loud banner prints whether or not we restart;
//   - the SERVER child (serverName, the air/go-run hot-reload process)
//     gets exactly ONE automatic restart — the rebuild race is
//     transient (the stale binary's listener needs a moment to die),
//     so a single retry usually revives the loop. The cap is hard: a
//     second death aborts, so a genuinely broken server can't flap
//     forever;
//   - any other child death (frontends) aborts immediately — npm dev
//     servers don't die from transient races;
//   - a child death racing a pending signal is treated as the signal
//     (Ctrl-C delivers SIGINT to the whole foreground process group,
//     so children die from the same keypress that lands on sigCh).
//
// restartServer may be nil (no server child was started); a death of
// any child then aborts like any other.
func runWaitLoop(
	sigCh <-chan os.Signal,
	devErrCh <-chan error,
	childExitCh <-chan *managedProcess,
	serverName string,
	restartServer func() error,
	announceExit func(*managedProcess),
) error {
	restartsLeft := 0
	if restartServer != nil && serverName != "" {
		restartsLeft = 1
	}
	for {
		select {
		case <-sigCh:
			fmt.Println("\n[run] Shutting down...")
			return nil
		case err := <-devErrCh:
			fmt.Fprintf(os.Stderr, "\n[run] dev proxy died: %v — shutting down\n", err)
			return fmt.Errorf("dev proxy died: %w", err)
		case p := <-childExitCh:
			// When both a signal and a child death are pending, the
			// signal is the truth and the death is its consequence.
			select {
			case <-sigCh:
				fmt.Println("\n[run] Shutting down...")
				return nil
			default:
			}
			announceExit(p)
			if p.name == serverName && restartsLeft > 0 {
				restartsLeft--
				fmt.Fprintf(os.Stderr, "[run] restarting %q (one automatic retry — hot-reload rebuild races are transient; a second death aborts the dev session)...\n", serverName)
				if rerr := restartServer(); rerr != nil {
					return fmt.Errorf("restart %q after unexpected exit: %w", serverName, rerr)
				}
				continue
			}
			return childExitError(p.name, p.exitErr)
		}
	}
}

// preflightPostgresPort fails fast — BEFORE `docker compose up` — when
// the postgres publish port is already owned by something that isn't
// this project's own postgres container. Without it, a host-installed
// postgres on 5432 surfaced as "failed to start infrastructure: exit
// status 1" with the POSTGRES_PORT remedy buried in a
// docker-compose.yml comment (journey fr-8236556f2e). The error spells
// out the exact rerun command with a verified-free port.
//
// composeOwnsPort reports whether the project's compose postgres
// container is already running (an idempotent `compose up -d` from a
// previous session) — in that case the busy port is OURS and compose
// up will succeed. Injected as a func so tests don't need docker.
func preflightPostgresPort(port int, composeOwnsPort func() bool) error {
	if port <= 0 || port > 65535 {
		// Malformed POSTGRES_PORT: let docker compose produce its own
		// error rather than second-guessing here.
		return nil
	}
	// The compose template publishes on 127.0.0.1 specifically, so the
	// probe matches that bind exactly. A host postgres on the wildcard
	// or on 127.0.0.1 both make this listen fail; a (weird) ::1-only
	// listener doesn't conflict with the v4 publish and passes.
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err == nil {
		_ = ln.Close()
		return nil
	}
	if composeOwnsPort != nil && composeOwnsPort() {
		return nil
	}
	return fmt.Errorf("port %d is already in use on 127.0.0.1 (a postgres already running on this host?), so docker compose cannot publish the dev database there.\n  Rerun with a free port:\n\n    POSTGRES_PORT=%d forge run\n", port, suggestFreeLoopbackPort(15432))
}

// composeHasRunningPostgres reports whether the compose project's
// postgres service has a RUNNING container — the one legitimate owner
// of the published port besides nobody.
func composeHasRunningPostgres(ctx context.Context, composePath string) bool {
	out, err := exec.CommandContext(ctx, "docker", "compose", "-f", composePath,
		"ps", "-q", "--status", "running", "postgres").Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// suggestFreeLoopbackPort asks the kernel for a currently-free loopback
// port to put in remedy messages. Falls back to the given default when
// the OS refuses (resource exhaustion); inherently racy, but the
// suggestion is re-verified the moment the user reruns with it.
func suggestFreeLoopbackPort(fallback int) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fallback
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// discoverComposeDatabaseURL asks docker compose for the published host
// port of the postgres service and composes a host-reachable
// DATABASE_URL from it. Returns "" when there is no postgres service,
// docker isn't available, or the output doesn't parse — the caller
// falls back to the binary's own defaults (and the generated bootstrap
// fails validateDeps loudly if a DB-dependent service can't be served).
//
// User/password/db default to the same values the scaffolded
// docker-compose.yml defaults to, honoring the same POSTGRES_* env
// overrides so the two can't drift.
func discoverComposeDatabaseURL(ctx context.Context, composePath, projectName string) string {
	out, err := exec.CommandContext(ctx, "docker", "compose", "-f", composePath, "port", "postgres", "5432").Output()
	if err != nil {
		return ""
	}
	return composePortToDatabaseURL(string(out),
		envOrDefault("POSTGRES_USER", "postgres"),
		envOrDefault("POSTGRES_PASSWORD", "postgres"),
		envOrDefault("POSTGRES_DB", projectName))
}

// composePortToDatabaseURL converts `docker compose port` output
// ("0.0.0.0:5432\n" / "127.0.0.1:49213") into a postgres DSN pointing
// at localhost. Returns "" for unparseable input.
func composePortToDatabaseURL(portOutput, user, password, dbName string) string {
	hostPort := strings.TrimSpace(portOutput)
	// Multi-line output (IPv4 + IPv6 mappings) — take the first line.
	if i := strings.IndexByte(hostPort, '\n'); i >= 0 {
		hostPort = strings.TrimSpace(hostPort[:i])
	}
	idx := strings.LastIndex(hostPort, ":")
	if idx < 0 {
		return ""
	}
	port := hostPort[idx+1:]
	if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
		return ""
	}
	return fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable", user, password, port, dbName)
}

// redactDSNPassword masks the password section of a user:pass@host DSN
// for log output.
func redactDSNPassword(dsn string) string {
	at := strings.Index(dsn, "@")
	scheme := strings.Index(dsn, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return dsn
	}
	creds := dsn[scheme+3 : at]
	if colon := strings.Index(creds, ":"); colon >= 0 {
		return dsn[:scheme+3] + creds[:colon] + ":***" + dsn[at:]
	}
	return dsn
}

// envOrDefault returns the env var's value or the default when unset/empty.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// teardownInfrastructure runs `docker compose down` if a project-level
// docker-compose.yml exists. Best-effort: errors are logged via the
// command's stderr inherit but never surfaced to the caller, since
// `forge run` is already shutting down.
//
// Uses a fresh Background context with a 30s timeout because runProjectDev
// only reaches this point after its own ctx has been cancelled by
// SIGINT/SIGTERM — passing that cancelled ctx to `docker compose down`
// would no-op the teardown.
func teardownInfrastructure() {
	composePath := "docker-compose.yml"
	if _, err := os.Stat(composePath); err != nil {
		return
	}
	fmt.Println("[run] Stopping infrastructure...")
	downCtx, downCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer downCancel()
	downCmd := exec.CommandContext(downCtx, "docker", "compose", "-f", composePath, "down")
	downCmd.Stdout = os.Stdout
	downCmd.Stderr = os.Stderr
	_ = downCmd.Run()
}

// filterServicesByNames returns only components whose name matches one of the given names.
func filterServicesByNames(services []config.ComponentConfig, names []string) []config.ComponentConfig {
	nameSet := make(map[string]struct{}, len(names))
	for _, n := range names {
		nameSet[n] = struct{}{}
	}
	var filtered []config.ComponentConfig
	for _, s := range services {
		if _, ok := nameSet[s.Name]; ok {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// filterFrontendsByNames returns only frontends whose name matches one of the given names.
func filterFrontendsByNames(frontends []config.FrontendConfig, names []string) []config.FrontendConfig {
	nameSet := make(map[string]struct{}, len(names))
	for _, n := range names {
		nameSet[n] = struct{}{}
	}
	var filtered []config.FrontendConfig
	for _, f := range frontends {
		if _, ok := nameSet[f.Name]; ok {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

// streamWithPrefix reads from r line by line and prints each line with a prefix.
// The provided mutex serialises output across concurrent goroutines so that
// lines from different streams are never interleaved.
func streamWithPrefix(prefix string, r io.Reader, mu *sync.Mutex) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		mu.Lock()
		fmt.Print(prefix + line + "\n")
		mu.Unlock()
	}
}

// envConfigToEnvVars projects a merged per-env config map onto a flat
// NAME→VALUE map suitable for passing to a child process.
//
// The keys of envCfg are proto field names (snake_case). We map them to
// uppercase env-var names by parsing proto/config/ for ConfigFieldOptions
// to honour any custom env_var: annotations. When the proto descriptor
// is unavailable (fresh project, no descriptor yet) we fall back to
// converting snake_case → SCREAMING_SNAKE.
//
// projectConfigPath is the path to forge.yaml; the parent dir is used
// to resolve proto/config/ for the annotation lookup. The surface
// deliberately takes the file path (not the dir) so callers can pass
// `findProjectConfigFile()`'s return value directly.
//
// Sensitive fields are skipped here — `forge run` is a local dev tool
// and shouldn't be plumbing secret refs through env vars. Set the
// secret value in your local env (.env / direnv) instead.
func envConfigToEnvVars(envCfg map[string]any, projectConfigPath string) map[string]string {
	out := map[string]string{}
	annotations := loadConfigAnnotations(filepath.Dir(projectConfigPath))

	for key, val := range envCfg {
		envVar := strings.ToUpper(key)
		var sensitive bool
		if ann, ok := annotations[key]; ok {
			if ann.EnvVar != "" {
				envVar = ann.EnvVar
			}
			sensitive = ann.Sensitive
		}
		if sensitive {
			continue
		}
		if s, ok := val.(string); ok {
			if _, isSecretRef := parseLooseSecretRef(s); isSecretRef {
				// Secret refs aren't resolvable at run-time. Skip and
				// expect the user to set them in their env.
				continue
			}
		}
		out[envVar] = stringifyEnvValue(val)
	}
	return out
}

// loadProjectConfigEnv loads the per-env config from the sibling
// `config.<env>.yaml` file and projects it to env-var strings via
// [envConfigToEnvVars]. Returns an empty map (not nil) on any error
// so callers can pass the result straight to [hostlaunch.LayerHostEnv]
// without guarding. Missing file / empty config is non-fatal —
// `forge run <svc>` runs against whatever defaults the binary's
// flag/env loader provides when no per-env config is declared.
//
// Reuses the same loader + projector as the orchestrator
// (runProjectDev) so host-mode services see the same per-env config
// values cluster-mode services get via the ConfigMap projection.
// Sensitive fields and ${SECRET_REF} placeholders are skipped — those
// belong in `.env.<env>` (the gitignored dotenv) or the developer
// shell, not in committed sibling-file config.
func loadProjectConfigEnv(_ *config.ProjectConfig, env string) map[string]string {
	if env == "" {
		return map[string]string{}
	}
	projectPath, perr := findProjectConfigFile()
	if perr != nil {
		return map[string]string{}
	}
	projectDir := filepath.Dir(projectPath)
	envCfg, lerr := config.LoadEnvironmentConfig(projectDir, env)
	if lerr != nil {
		return map[string]string{}
	}
	return envConfigToEnvVars(envCfg, projectPath)
}

// configAnnotation is a lightweight projection of ConfigField used by
// the run command to map proto field names to env-var names.
type configAnnotation struct {
	EnvVar    string
	Sensitive bool
}

// loadConfigAnnotations parses proto/config/ via the forge descriptor
// and returns proto-field-name → annotation. Returns an empty map on
// any error (the caller falls back to snake→SCREAMING_SNAKE).
func loadConfigAnnotations(projectDir string) map[string]configAnnotation {
	out := map[string]configAnnotation{}
	messages, err := codegen.ParseConfigProtosFromDir(filepath.Join(projectDir, "proto", "config"))
	if err != nil || len(messages) == 0 {
		return out
	}
	for _, m := range messages {
		for _, f := range m.Fields {
			out[f.Name] = configAnnotation{EnvVar: f.EnvVar, Sensitive: f.Sensitive}
		}
	}
	return out
}

// parseLooseSecretRef returns ("name", true) for "${name}" strings.
// Used by run to detect un-resolvable secret references in dev config
// that should be skipped (let the user's local env supply the value).
func parseLooseSecretRef(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") {
		return "", false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(s, "${"), "}")
	if inner == "" {
		return "", false
	}
	return inner, true
}

// stringifyEnvValue turns a YAML-decoded scalar into its env-var string form.
func stringifyEnvValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	default:
		return fmt.Sprint(v)
	}
}

// runHostService executes a single service as a host process — the
// inner loop for services declared `deploy = "host"` in the env's
// rendered KCL. When KCL declares deploy.Host.Runner, dispatch picks
// it up (go-run / air / binary / delve); otherwise falls back to
// `go run ./cmd server <service>` for backwards compat.
//
// Foreground mode streams stdout/stderr with a `[<service>]` prefix and
// blocks until Ctrl-C. Background mode (background=true) detaches the
// subprocess, writes its PID to ~/.cache/forge/run/<service>.pid, and
// returns immediately so orchestration scripts (Taskfile, cloud-dev.sh)
// can continue. Stop the background process with `forge run <service>
// stop`.
//
// Env composition (precedence: later overrides earlier on key
// conflict; base os.Environ() wins last across the whole chain):
//
//  1. os.Environ() — the parent process env. Wins last so a developer
//     shell override (`LOG_LEVEL=trace forge run ...`) beats everything
//     else.
//  2. `config.<env>.yaml` sibling file — non-secret per-env config
//     (environment / log_format / log_level / etc.). Same source the
//     cluster-mode ConfigMap projection reads, layered here so
//     host-mode services don't drift. Sensitive fields and
//     ${SECRET_REF} placeholders are skipped (host-mode dev expects
//     those in .env.<env> or the developer shell). Lowest precedence
//     among extras. (Pre-`environments-to-kcl` migration this lived in
//     `forge.yaml`'s removed `environments[<env>].config` block.)
//  3. secretsFile (`.env.<env>`) — gitignored dotenv of API keys plus
//     any developer-local overrides. Empty path → skip; missing file →
//     warn and continue; unreadable (parse / permission) → error. Wins
//     over per-env config so a developer can shadow a committed value
//     without editing tracked files.
//  4. host.EnvVars (KCL) — KCL-declared per-env config. Wins over the
//     two layers above so reproducible per-env config can't drift
//     across machines.
//
// secretsFile, when empty, falls back to host.SecretsFile from KCL.
// No legacy default (".env.<env>") — projects on the new shape must
// declare SecretsFile in KCL to opt in.
func runHostService(ctx context.Context, name, env, secretsFile string, background bool) error {
	store, err := loadProjectStore()
	if err != nil {
		return err
	}
	cfg := store.Config()
	if name == "" {
		return fmt.Errorf("service name required (usage: forge run <service>)")
	}

	// Verify the service exists in forge.yaml. The KCL `deploy:` field
	// (when declared) is the source of truth for host vs cluster
	// placement, but `forge run <svc>` doesn't gate on it — running a
	// cluster-mode service locally is fine for ad-hoc debugging.
	var svc *config.ComponentConfig
	for i := range cfg.Components {
		if cfg.Components[i].Name == name {
			svc = &cfg.Components[i]
			break
		}
	}
	if svc == nil {
		return fmt.Errorf("service %q not found in forge.yaml (declared components: %s)",
			name, strings.Join(declaredServiceNames(cfg), ", "))
	}
	// Registration check (pkg/app/services.go): the binary serves only
	// the rows the user lists there. Best-effort — a missing/broken
	// registry falls open; the generated BootstrapOnly guard backstops.
	if cfgPath, perr := findProjectConfigFile(); perr == nil && isConnectServiceConfig(*svc) {
		if reg, regErr := loadServiceRegistry(filepath.Dir(cfgPath)); regErr == nil && !reg.registered(name) {
			return fmt.Errorf("service %q is not registered in %s — this binary does not serve it; add its serviceRow line to RegisteredServices to serve it here", name, serviceRegistryRelPath)
		}
	}

	// Look up the KCL HostDeploy block for this service. When the env's
	// KCL declares it, the Runner / AirConfig / DelvePort / SecretsFile
	// / EnvVars fields drive dispatch + env composition; otherwise nil
	// falls back to the legacy `go run ./cmd server <svc>` shape for
	// projects that haven't migrated to the deploy module yet.
	host := lookupKCLHostDeploy(ctx, env, name)

	if secretsFile == "" && host != nil {
		secretsFile = host.SecretsFile
	}
	secrets, loadErr := hostlaunch.LoadSecretsFile(secretsFile)
	switch {
	case loadErr == nil && secretsFile != "":
		fmt.Printf("[run] %s: loaded %d secrets from %s\n", name, len(secrets), secretsFile)
	case loadErr != nil && os.IsNotExist(loadErr):
		fmt.Printf("[run] %s: warning: secrets file %s missing; continuing without it\n", name, secretsFile)
	case loadErr != nil:
		return fmt.Errorf("read secrets file %s: %w", secretsFile, loadErr)
	}

	// Layer KCL-declared env_vars on top of the loaded secrets. EnvVars
	// only carries inline `value` fields at this surface — secret_ref /
	// config_map_ref shapes are for K8sCluster / cluster projection and
	// have no meaningful host equivalent. Skip non-value entries.
	envVars := hostEnvVarsToMap(host)
	if len(envVars) > 0 {
		fmt.Printf("[run] %s: layering %d KCL env_vars on top of secrets\n", name, len(envVars))
	}

	// Load the `config.<env>.yaml` sibling and project to env-var
	// strings. Same source as the cluster ConfigMap projection — without
	// this layer, host-mode services see ONLY .env.<env> (the gitignored
	// dotenv) and the per-env config cluster-mode services see via
	// ConfigMap is invisible on the host. Missing env / empty config is
	// non-fatal: we log it and continue with whatever the binary's
	// startup defaults provide.
	projectConfigEnv := loadProjectConfigEnv(cfg, env)
	if len(projectConfigEnv) > 0 {
		fmt.Printf("[run] %s: layering %d forge.yaml config values for env %q\n", name, len(projectConfigEnv), env)
	}

	cmd := buildRunHostCmd(ctx, name, host)
	cmd.Env = hostlaunch.LayerHostEnv(os.Environ(), projectConfigEnv, secrets, envVars)

	// Pid file path is shared by foreground (cleanup on exit) and
	// background (handle for `forge run <svc> stop`).
	pidPath, perr := hostRunPIDPath(name)
	if perr != nil {
		return perr
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return fmt.Errorf("create pid dir: %w", err)
	}

	if background {
		// Background mode: detach. We don't pipe stdout/stderr — the
		// caller usually doesn't want them anyway (orchestration script
		// shapes), and capturing into a log file is a future fight.
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}
		pid := cmd.Process.Pid
		if writeErr := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", pid)), 0o644); writeErr != nil {
			fmt.Printf("[run] warning: write pid file %s: %v\n", pidPath, writeErr)
		}
		_ = cmd.Process.Release()
		fmt.Printf("[run] %s: detached (pid=%d). Stop with `forge run %s stop`.\n", name, pid, name)
		fmt.Printf("[run] pid file: %s\n", pidPath)
		return nil
	}

	// Foreground mode: stream output with a prefix and clean up on
	// Ctrl-C. Mirrors the orchestrator's streamWithPrefix helper.
	prefix := fmt.Sprintf("[%s] ", name)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	pid := cmd.Process.Pid
	_ = os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", pid)), 0o644)
	defer func() { _ = os.Remove(pidPath) }()

	var outputMu sync.Mutex
	go streamWithPrefix(prefix, stdout, &outputMu)
	go streamWithPrefix(prefix, stderr, &outputMu)

	fmt.Printf("[run] %s: started (pid=%d). Ctrl-C to stop.\n", name, pid)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	select {
	case <-sigCh:
		fmt.Printf("\n[run] %s: stopping (pid=%d)...\n", name, pid)
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-doneCh:
		case <-time.After(10 * time.Second):
			fmt.Printf("[run] %s: did not exit after SIGTERM, killing.\n", name)
			_ = cmd.Process.Kill()
			<-doneCh
		}
	case err := <-doneCh:
		if err != nil {
			return fmt.Errorf("%s exited: %w", name, err)
		}
	}
	return nil
}

// runHostServiceStop reads the per-service PID file and sends SIGTERM.
// No-op (with a friendly notice) when nothing is tracked, so callers
// can invoke this unconditionally on teardown.
func runHostServiceStop(name string) error {
	if name == "" {
		return fmt.Errorf("service name required (usage: forge run <service> stop)")
	}
	pidPath, err := hostRunPIDPath(name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("[run] %s: no tracked process (pid file %s missing)\n", name, pidPath)
			return nil
		}
		return fmt.Errorf("read pid file: %w", err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
		return fmt.Errorf("parse pid file %s: %w", pidPath, err)
	}
	proc, ferr := os.FindProcess(pid)
	if ferr != nil {
		fmt.Printf("[run] %s: find pid %d: %v\n", name, pid, ferr)
	} else if err := proc.Signal(syscall.SIGTERM); err != nil {
		fmt.Printf("[run] %s: signal pid %d: %v (already exited?)\n", name, pid, err)
	} else {
		fmt.Printf("[run] %s: SIGTERM sent to pid %d\n", name, pid)
	}
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("[run] warning: remove pid file %s: %v\n", pidPath, err)
	}
	return nil
}

// hostRunPIDPath is a thin shim over hostlaunch.PIDPath kept so the
// existing in-package callers and the run_test.go TestHostRunPIDPath
// helper stay terse. Canonical path:
//
//	$HOME/.cache/forge/run/<service>.pid
func hostRunPIDPath(name string) (string, error) {
	return hostlaunch.PIDPath(name)
}

// lookupKCLHostDeploy reads the env's rendered KCL and returns the
// HostDeploy block for the named service. Returns nil when the KCL
// render fails (e.g. agent A's module not installed yet) or the
// service isn't declared host-mode in this env. Errors from the KCL
// render are silently dropped — the caller falls back to the legacy
// go-run shape.
func lookupKCLHostDeploy(ctx context.Context, env, svcName string) *HostDeploy {
	if env == "" {
		return nil
	}
	projectDir := projectDirForKCL()
	entities, err := RenderKCL(ctx, projectDir, env)
	if err != nil {
		return nil
	}
	svc := entities.FindService(svcName)
	if svc == nil || svc.Deploy.Type != "host" || svc.Deploy.Host == nil {
		return nil
	}
	return svc.Deploy.Host
}

// buildRunHostCmd composes the exec.Cmd for `forge run <svc>` based on
// the KCL-declared runner. Thin shim over hostlaunch.BuildCmd; a nil
// host falls through to the legacy `go run ./cmd server <svc>` shape
// (hostlaunch's unknown/empty-runner default), preserving the
// pre-collapse behaviour for projects without a KCL deploy block.
//
// The runner dispatch matrix (go-run / air / binary / delve) and its
// defaults (DefaultAirConfig=".air.toml", DefaultDelvePort=2345) live
// in the hostlaunch package alongside the `forge up` host phase.
func buildRunHostCmd(ctx context.Context, name string, host *HostDeploy) *exec.Cmd {
	spec := hostlaunch.RunnerSpec{}
	if host != nil {
		spec = hostlaunch.RunnerSpec{
			Runner:     host.Runner,
			AirConfig:  host.AirConfig,
			DelvePort:  host.DelvePort,
			WorkingDir: host.WorkingDir,
			ProjectDir: projectDirForKCL(),
		}
	}
	return hostlaunch.BuildCmd(ctx, name, spec)
}

// hostEnvVarsToMap projects the HostDeploy.EnvVars slice to a flat
// NAME→VALUE map for layering onto the subprocess env.
//
// Only the inline `value` channel applies on the host — KCLEnvVar's
// other channels (secret_ref, config_map_ref) are cluster-mode
// projections (Deployment.env.valueFrom.secretKeyRef etc.) with no
// meaningful host equivalent. Those projection channels stay in KCL
// for K8sCluster services; on the host, secrets come from the
// gitignored secrets_file.
//
// Returns an empty map (not nil) on a nil host, so callers can pass
// the result straight to [hostlaunch.LayerHostEnv] without guarding.
func hostEnvVarsToMap(host *HostDeploy) map[string]string {
	if host == nil || len(host.EnvVars) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(host.EnvVars))
	for _, ev := range host.EnvVars {
		if ev.Name == "" || ev.Value == "" {
			continue
		}
		out[ev.Name] = ev.Value
	}
	return out
}

// declaredServiceNames returns the names of every service in forge.yaml,
// used by the error path of runHostService to point users at the right
// spelling when they typo a service name.
func declaredServiceNames(cfg *config.ProjectConfig) []string {
	out := make([]string, 0, len(cfg.Components))
	for _, s := range cfg.Components {
		out = append(out, s.Name)
	}
	return out
}

// resolveProxyPort picks the bind port for the cross-frontend dev
// proxy. Precedence: explicit --proxy-port flag > FORGE_RUN_PROXY_PORT
// env > [defaultDevProxyPort]. A malformed env value is silently
// ignored — the proxy is best-effort and we don't want a typo in
// the user's shell to block `forge run`.
//
// declared maps every port a service/frontend child will bind to a
// human-readable component name (see declaredComponentPorts). The
// proxy must never share a port with a component it fronts: the proxy
// binds loopback-only while the Go server binds the wildcard, so both
// binds can SUCCEED on the same number (macOS) and the browser's
// localhost then races between them by address family — the
// split-brain of journey fr-5b2121e48f. The default skips past
// declared ports silently; an explicit flag/env collision is an error,
// because honoring it would guarantee the split-brain.
func resolveProxyPort(flagPort int, declared map[int]string) (int, error) {
	pick, source := 0, ""
	if flagPort > 0 {
		pick, source = flagPort, "--proxy-port"
	} else if env := os.Getenv("FORGE_RUN_PROXY_PORT"); env != "" {
		if p, err := strconv.Atoi(env); err == nil && p > 0 {
			pick, source = p, "FORGE_RUN_PROXY_PORT"
		}
	}
	if pick > 0 {
		if owner, clash := declared[pick]; clash {
			return 0, fmt.Errorf("dev proxy port %d (%s) is also %s's listen port — the proxy and the backend would split the port across address families and the browser's localhost would race between them; pick a port no component declares", pick, source, owner)
		}
		return pick, nil
	}
	for p := defaultDevProxyPort; p < defaultDevProxyPort+200; p++ {
		if _, clash := declared[p]; clash {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("no free dev proxy port in %d-%d (every candidate is a declared component port); set --proxy-port explicitly", defaultDevProxyPort, defaultDevProxyPort+199)
}

// declaredComponentPorts maps every port a service or frontend child
// will bind to a human-readable component name, for the dev proxy's
// overlap avoidance. First declaration wins on (misconfigured)
// duplicate ports — any name is enough for the conflict message.
func declaredComponentPorts(services []config.ComponentConfig, frontends []config.FrontendConfig) map[int]string {
	out := map[int]string{}
	for _, s := range services {
		if p := s.PrimaryPort(); p > 0 {
			if _, dup := out[p]; !dup {
				out[p] = "service " + s.Name
			}
		}
	}
	for _, f := range frontends {
		if f.Port > 0 {
			if _, dup := out[f.Port]; !dup {
				out[f.Port] = "frontend " + f.Name
			}
		}
	}
	return out
}

// loadDevProxyRoutes is the KCL-render shim for the run command's
// dev proxy. Returns nil when KCL isn't available / the env isn't
// rendered yet — the caller falls back to the implicit
// `<name>.localhost` per-frontend default. Mirrors the
// lookupKCLHostDeploy shape (silent fallback) — the proxy is a
// developer convenience, not a correctness gate.
func loadDevProxyRoutes(ctx context.Context, env string) []HTTPRouteEntity {
	if env == "" {
		return nil
	}
	projectDir := projectDirForKCL()
	entities, err := RenderKCL(ctx, projectDir, env)
	if err != nil || entities == nil {
		return nil
	}
	return entities.HTTPRoutes
}

// buildDevProxyBackends composes the proxy's dispatch table. Each
// frontend contributes a default `<name>.localhost` → fe.Port entry;
// HTTPRoute entities then override or add hostnames so the dev proxy
// matches the production HTTPRoute hostnames (prod-parity).
//
// Service-side HTTPRoutes need a host:port pair: HTTPRouteEntity
// carries `Service` (backend service name) and `Port`, so we resolve
// the loopback port directly from the route's `Port` field. When the
// route declares no `Host` the entry is skipped — root-path routes
// like `/` are the discouraged path-prefix shape and out of scope
// for the host-based proxy (basePath gotcha; see SKILL.md).
func buildDevProxyBackends(
	frontends []config.FrontendConfig,
	services []config.ComponentConfig,
	routes []HTTPRouteEntity,
) []devproxy.Backend {
	// Index frontends by name for HTTPRoute lookups + a default
	// host-table entry per frontend.
	byName := make(map[string]struct {
		port int
		kind string
	}, len(frontends)+len(services))
	for _, fe := range frontends {
		if fe.Port > 0 {
			byName[fe.Name] = struct {
				port int
				kind string
			}{port: fe.Port, kind: "frontend"}
		}
	}
	for _, svc := range services {
		if p := svc.PrimaryPort(); p > 0 {
			byName[svc.Name] = struct {
				port int
				kind string
			}{port: p, kind: "service"}
		}
	}

	// Track hosts we've already added so HTTPRoute entries win the
	// last-write semantics over the auto-generated frontend default.
	added := map[string]struct{}{}
	var out []devproxy.Backend

	// Default `<name>.localhost` per frontend. Gives the unified URL
	// out of the box even before the user declares any HTTPRoutes.
	for _, fe := range frontends {
		if fe.Port <= 0 {
			continue
		}
		host := fe.Name + ".localhost"
		added[host] = struct{}{}
		out = append(out, devproxy.Backend{
			Host: host,
			Port: fe.Port,
			Kind: "frontend",
			Name: fe.Name,
		})
	}

	// HTTPRoute-declared hosts. Resolve the backend port from the
	// route's `Service` ref via byName so a service whose port the
	// HTTPRoute mis-declares falls back to the forge.yaml port. (The
	// HTTPRoute.Port and forge.yaml port SHOULD agree — `forge audit
	// ingress` is the cross-check — but we prefer the forge.yaml port
	// here since that's what the binary actually binds to.)
	for _, rt := range routes {
		if rt.Host == "" {
			continue
		}
		info, ok := byName[rt.Service]
		if !ok || info.port == 0 {
			// Route references a backend we don't run as a host
			// process — cluster-only services from a mixed-mode env.
			// Skip silently; the audit command surfaces these.
			continue
		}
		host := strings.ToLower(rt.Host)
		if _, dup := added[host]; dup {
			continue
		}
		added[host] = struct{}{}
		out = append(out, devproxy.Backend{
			Host: host,
			Port: info.port,
			Kind: info.kind,
			Name: rt.Service,
		})
	}

	return out
}

// printDevProxyBanner writes the one-line proxy summary the user sees
// after `forge run` finishes its startup phase. The format is stable
// for screenshot/grep purposes — `[run] Dev URL: http://localhost:<port>
// (<host1>:<port>, <host2>:<port>, ...)` — and matches the shape the
// task brief specifies.
func printDevProxyBanner(port int, backends []devproxy.Backend) {
	hosts := make([]string, 0, len(backends))
	for _, b := range backends {
		hosts = append(hosts, fmt.Sprintf("%s:%d", b.Host, port))
	}
	fmt.Printf("\n[run] Dev URL: http://localhost:%d (%s)\n", port, strings.Join(hosts, ", "))
}
