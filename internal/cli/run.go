package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/reliant-labs/forge/internal/config"
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
}

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
		if opts.debug {
			cmd.Env = append(os.Environ(), "ENVIRONMENT=development")
		}
		cmd.Dir = "."
		if err := startProcess(cfg.Name, cmd); err != nil {
			fmt.Printf("[run] Warning: %v\n", err)
		}
	}

	// 3. Start Next.js frontends.
	for _, fe := range frontendsToRun {
		cmd := exec.CommandContext(ctx, "npm", "run", "dev")
		cmd.Dir = fe.Path
		if err := startProcess(fe.Name, cmd); err != nil {
			fmt.Printf("[run] Warning: %v\n", err)
		}
	}

	if len(processes) == 0 {
		fmt.Println("[run] No services or frontends to run.")
		return nil
	}

	fmt.Printf("\n[run] %d process(es) started. Press Ctrl+C to stop.\n\n", len(processes))

	// Wait for signal.
	<-sigCh
	fmt.Println("\n[run] Shutting down...")
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

	// Tear down infrastructure (unless --no-infra).
	if !opts.noInfra {
		composePath := "docker-compose.yml"
		if _, err := os.Stat(composePath); err == nil {
			fmt.Println("[run] Stopping infrastructure...")
			downCmd := exec.Command("docker", "compose", "-f", composePath, "down")
			downCmd.Stdout = os.Stdout
			downCmd.Stderr = os.Stderr
			_ = downCmd.Run()
		}
	}

	fmt.Println("[run] All processes stopped.")
	return nil
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