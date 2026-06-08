package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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
// 8080 is the conventional dev-only port — distinct from any
// Next.js / service default (3000 / 8000 / 50051), so the proxy
// never collides with a backend's own port.
const defaultDevProxyPort = 8080

// runProjectDev orchestrates the local development environment.
func runProjectDev(opts runOptions) error {
	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}

	fmt.Printf("[run] Starting project: %s (env: %s)\n", cfg.Name, opts.env)
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

	// startProcess starts a command and registers it for cleanup.
	startProcess := func(name string, cmd *exec.Cmd) error {
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

		mu.Lock()
		processes = append(processes, &managedProcess{name: name, cmd: cmd})
		mu.Unlock()

		// Stream output in background goroutines.
		go streamWithPrefix(prefix, stdout, &outputMu)
		go streamWithPrefix(prefix, stderr, &outputMu)

		return nil
	}

	// Filter services/frontends based on --service flag
	servicesToRun := cfg.Services
	frontendsToRun := cfg.Frontends
	if len(opts.services) > 0 {
		servicesToRun = filterServicesByNames(cfg.Services, opts.services)
		frontendsToRun = filterFrontendsByNames(cfg.Frontends, opts.services)
		if len(servicesToRun) == 0 && len(frontendsToRun) == 0 {
			return fmt.Errorf("none of the specified services %v found in project config", opts.services)
		}
	}

	// 1. Start infrastructure via docker compose (unless --no-infra).
	if !opts.noInfra {
		composePath := "docker-compose.yml"
		if _, err := os.Stat(composePath); err == nil {
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
		}
	}

	// 2. Start Go binary via Air (hot reload) or go run fallback.
	// Single binary architecture: one process with service names as args.
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
		} else if cfg.HotReload {
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
		// Layer per-env config (forge.yaml/sibling) onto the subprocess
		// environment so the binary's flag/env loader sees the values.
		// Existing process env wins (a developer can still override
		// inline) — we apply the per-env values first, then anything
		// already set in os.Environ().
		baseEnv := hostlaunch.MergeEnv(envExtraEnv, os.Environ())
		if opts.debug {
			baseEnv = append(baseEnv, "ENVIRONMENT=development")
			cmd.Env = baseEnv
		} else if len(envExtraEnv) > 0 {
			cmd.Env = baseEnv
		}
		cmd.Dir = "."
		if err := startProcess(cfg.Name, cmd); err != nil {
			fmt.Printf("[run] Warning: %v\n", err)
		}
	}

	// 3. Start Next.js frontends. PORT is force-injected from the
	// forge.yaml-declared frontend port so the proxy can dispatch to
	// a deterministic loopback port even if a stale PORT bled in
	// from the parent shell. Mirrors the `forge up` host-mode shape
	// (see buildFrontendCmd in up.go).
	for _, fe := range frontendsToRun {
		cmd := exec.CommandContext(ctx, "npm", "run", "dev")
		cmd.Dir = fe.Path
		if fe.Port > 0 {
			cmd.Env = withForcedEnv(os.Environ(), "PORT", strconv.Itoa(fe.Port))
		}
		if err := startProcess(fe.Name, cmd); err != nil {
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
	// devGoroutineErrCh carries failures from background dev-proxy
	// goroutines back to the main loop. Pre-2026-06-07 these were swallowed
	// (a plain `fmt.Printf` inside the goroutine), so a bind failure like
	// EADDRINUSE on the proxy port printed a single line and then the
	// process happily reported "N process(es) started" while the proxy was
	// in fact dead — every frontend request returned ECONNREFUSED.
	//
	// Buffered to the number of goroutines we might spawn so a fast-failing
	// send never blocks. We act on the FIRST error (abort dev-up); later
	// errors from sibling goroutines are drained-and-logged so the user
	// sees them too, but only the first triggers the abort.
	devGoroutineErrCh := make(chan error, 2)
	if !opts.noProxy {
		proxyPort := resolveProxyPort(opts.proxyPort)
		routes := loadDevProxyRoutes(ctx, opts.env)
		backends := buildDevProxyBackends(frontendsToRun, servicesToRun, routes)
		if len(backends) == 0 {
			fmt.Println("[run] Dev proxy: no frontends or HTTP-routed services declared; skipping.")
		} else {
			router := devproxy.New(backends)
			addr := fmt.Sprintf("localhost:%d", proxyPort)
			srv := &http.Server{Addr: addr, Handler: router, ReadHeaderTimeout: 10 * time.Second}
			printDevProxyBanner(proxyPort, backends)
			go func() {
				if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					// Non-blocking: if the channel is full, the first
					// error already won — drop ours rather than block the
					// goroutine on a reader that's no longer waiting.
					select {
					case devGoroutineErrCh <- fmt.Errorf("dev proxy listener on %s: %w", addr, err):
					default:
					}
				}
			}()
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

	fmt.Printf("\n[run] %d process(es) started. Press Ctrl+C to stop.\n\n", len(processes))

	// Startup race: a bind failure (EADDRINUSE) hits ListenAndServe
	// synchronously, so the goroutine sends on devGoroutineErrCh within
	// a few hundred microseconds of Start. Give it a short window to
	// surface before we block on sigCh — without this, the user sees
	// "N process(es) started" first and the "dev proxy listener:" error
	// line trails behind, making the proxy look like it might recover.
	select {
	case err := <-devGoroutineErrCh:
		fmt.Fprintf(os.Stderr, "[run] FATAL: %v\n", err)
		cancel()
		return fmt.Errorf("dev startup aborted: %w", err)
	case <-time.After(250 * time.Millisecond):
		// Proxy bound cleanly (or the goroutine is still in flight; a
		// later failure during the lifetime of the dev session is rare
		// — proxy errors after bind are usually per-request, surfaced
		// to the client, not to stdout).
	}

	// Wait for signal OR a late-arriving dev-goroutine error. Late
	// goroutine errors are rare (post-bind ListenAndServe failures only
	// fire on accept errors that mean the OS revoked the listener
	// socket — typically EMFILE or similar), but surfacing them is
	// strictly better than the pre-refactor silent-drop.
	select {
	case <-sigCh:
		fmt.Println("\n[run] Shutting down...")
	case err := <-devGoroutineErrCh:
		fmt.Fprintf(os.Stderr, "\n[run] dev proxy died: %v — shutting down\n", err)
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

	// Wait for processes to exit with a single global timeout.
	// Each Wait() runs in its own goroutine so the 10s budget applies to
	// the whole set, not per-process (O(N*10s) worst case before).
	type waitResult struct {
		proc *managedProcess
		done chan struct{}
	}
	waits := make([]waitResult, 0, len(toStop))
	var shutdownWG sync.WaitGroup
	for _, p := range toStop {
		p := p
		done := make(chan struct{})
		waits = append(waits, waitResult{proc: p, done: done})
		shutdownWG.Add(1)
		go func() {
			defer shutdownWG.Done()
			_ = p.cmd.Wait()
			close(done)
		}()
	}

	allDone := make(chan struct{})
	go func() {
		shutdownWG.Wait()
		close(allDone)
	}()

	select {
	case <-allDone:
		// Every process exited cleanly within the shared budget.
	case <-time.After(10 * time.Second):
		// Single global timeout reached — SIGKILL anything still running
		// in one pass, then wait for the forced exits to flush.
		for _, w := range waits {
			select {
			case <-w.done:
				// Already exited.
			default:
				if w.proc.cmd.Process != nil {
					fmt.Printf("[run]   %s (pid %d) did not exit after SIGTERM, killing...\n", w.proc.name, w.proc.cmd.Process.Pid)
					_ = w.proc.cmd.Process.Kill()
				}
			}
		}
		<-allDone
	}

	if !opts.noInfra {
		teardownInfrastructure()
	}

	fmt.Println("[run] All processes stopped.")
	return nil
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

// filterServicesByNames returns only services whose name matches one of the given names.
func filterServicesByNames(services []config.ServiceConfig, names []string) []config.ServiceConfig {
	nameSet := make(map[string]struct{}, len(names))
	for _, n := range names {
		nameSet[n] = struct{}{}
	}
	var filtered []config.ServiceConfig
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
	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("service name required (usage: forge run <service>)")
	}

	// Verify the service exists in forge.yaml. The KCL `deploy:` field
	// (when declared) is the source of truth for host vs cluster
	// placement, but `forge run <svc>` doesn't gate on it — running a
	// cluster-mode service locally is fine for ad-hoc debugging.
	var svc *config.ServiceConfig
	for i := range cfg.Services {
		if cfg.Services[i].Name == name {
			svc = &cfg.Services[i]
			break
		}
	}
	if svc == nil {
		return fmt.Errorf("service %q not found in forge.yaml (declared services: %s)",
			name, strings.Join(declaredServiceNames(cfg), ", "))
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
	out := make([]string, 0, len(cfg.Services))
	for _, s := range cfg.Services {
		out = append(out, s.Name)
	}
	return out
}

// resolveProxyPort picks the bind port for the cross-frontend dev
// proxy. Precedence: explicit --proxy-port flag > FORGE_RUN_PROXY_PORT
// env > [defaultDevProxyPort]. A malformed env value is silently
// ignored — the proxy is best-effort and we don't want a typo in
// the user's shell to block `forge run`.
func resolveProxyPort(flagPort int) int {
	if flagPort > 0 {
		return flagPort
	}
	if env := os.Getenv("FORGE_RUN_PROXY_PORT"); env != "" {
		if p, err := strconv.Atoi(env); err == nil && p > 0 {
			return p
		}
	}
	return defaultDevProxyPort
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
	services []config.ServiceConfig,
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
		if svc.Port > 0 {
			byName[svc.Name] = struct {
				port int
				kind string
			}{port: svc.Port, kind: "service"}
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

