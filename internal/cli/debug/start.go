package debug

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/factory"
	dbgsvc "github.com/reliant-labs/forge/internal/debug"
)

func newStartCmd(f *factory.Factory) *cobra.Command {
	var (
		attachPID  int
		port       int
		jsonOutput bool
		dockerMode bool
	)

	cmd := &cobra.Command{
		Use:   "start <service>",
		Short: "Start a debug session for a service",
		Long: `Start a debug session for a Go service.

The binary is built with debug flags (-gcflags=all=-N -l) and launched under Delve.

If the argument contains "/" or ".", it is treated as a direct path.
Otherwise it is looked up by name in forge.yaml.

Examples:
  forge debug start api-gateway
  forge debug start --attach 12345
  forge debug start --port 2345 api-gateway
  forge debug start ./cmd/server`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if dockerMode {
				return runDebugStartDocker(cmd.Context())
			}
			if attachPID > 0 {
				return runDebugStartAttach(cmd.Context(), attachPID, jsonOutput)
			}
			if len(args) == 0 {
				return fmt.Errorf("provide a service name or path, or use --attach <pid>")
			}
			return runDebugStartService(cmd.Context(), f, args[0], port, jsonOutput)
		},
	}

	cmd.Flags().IntVar(&attachPID, "attach", 0, "Attach to an existing process by PID")
	cmd.Flags().IntVar(&port, "port", 0, "Debugger listen port (0 = auto)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&dockerMode, "docker", false, "Start debug session in Docker container")

	return cmd
}

func runDebugStartAttach(ctx context.Context, pid int, jsonOutput bool) error {
	d := dbgsvc.NewDelveDebugger()
	if err := d.StartAttach(ctx, pid); err != nil {
		return fmt.Errorf("attaching to PID %d: %w", pid, err)
	}

	session := &dbgsvc.SessionInfo{
		Type:    "delve",
		Addr:    d.Addr(),
		PID:     d.PID(),
		Binary:  fmt.Sprintf("pid:%d", pid),
		Started: time.Now(),
	}
	if err := debugSvc().SaveSession(".", session); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(session)
	} else {
		fmt.Printf("Attached to PID %d\n", pid)
		fmt.Printf("Delve listening at %s\n", d.Addr())
	}
	return nil
}

func runDebugStartService(ctx context.Context, f *factory.Factory, target string, port int, jsonOutput bool) error {
	buildPath := target
	serviceName := target

	if serviceName == "." || serviceName == "./" {
		serviceName = "app"
	}
	serviceName = filepath.Base(serviceName)

	// If the target doesn't look like a path, resolve it from project config.
	if !strings.Contains(target, "/") && !strings.Contains(target, ".") {
		store, err := f.LoadProjectStore()
		if err != nil {
			return fmt.Errorf("loading project config: %w", err)
		}
		found := false
		for _, svc := range store.Components() {
			if svc.Name == target {
				candidate := filepath.Join(svc.Path, "cmd", "server")
				if _, err := os.Stat(candidate); err == nil {
					buildPath = "./" + candidate
				} else if _, err := os.Stat("cmd"); err == nil {
					// Mono-service layout: top-level cmd/ directory.
					buildPath = "./cmd/..."
				} else {
					buildPath = "./" + svc.Path
				}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("service %q not found in project config; provide a path instead", target)
		}
	}

	// Build with debug flags.
	outputBinary := filepath.Join(".forge", "debug", serviceName)
	if err := os.MkdirAll(filepath.Dir(outputBinary), 0o755); err != nil {
		return fmt.Errorf("creating debug output dir: %w", err)
	}

	fmt.Printf("Building %s with debug flags...\n", buildPath)
	buildCmd := exec.CommandContext(ctx, "go", "build", "-gcflags=all=-N -l", "-o", outputBinary, buildPath)
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("building debug binary: %w", err)
	}

	absBinary, err := filepath.Abs(outputBinary)
	if err != nil {
		return fmt.Errorf("resolving binary path: %w", err)
	}

	// Determine binary args. If the binary was built from a cmd/ package
	// that contains a cobra "server" subcommand, pass it so the app
	// starts its HTTP listener rather than printing usage.
	var binArgs []string
	if _, err := os.Stat("cmd/server.go"); err == nil {
		binArgs = []string{"server"}
	}

	d := dbgsvc.NewDelveDebugger()
	if err := d.Start(ctx, absBinary, binArgs, port); err != nil {
		return fmt.Errorf("starting Delve: %w", err)
	}

	session := &dbgsvc.SessionInfo{
		Type:    "delve",
		Addr:    d.Addr(),
		PID:     d.PID(),
		Binary:  absBinary,
		Started: time.Now(),
	}
	if err := debugSvc().SaveSession(".", session); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(session)
	} else {
		fmt.Printf("Debug session started for %s\n", serviceName)
		fmt.Printf("Delve listening at %s\n", d.Addr())
	}
	return nil
}

func runDebugStartDocker(ctx context.Context) error {
	// Start the debug container.
	startCmd := exec.CommandContext(ctx, "docker", "compose", "--profile", "debug", "up", "-d", "app-debug")
	startCmd.Stdout = os.Stdout
	startCmd.Stderr = os.Stderr
	if err := startCmd.Run(); err != nil {
		return fmt.Errorf("starting debug container: %w", err)
	}

	// Wait for container to be running.
	fmt.Println("Waiting for debug container...")
	time.Sleep(5 * time.Second)

	// Discover Delve port.
	addr, err := discoverDelvePort(ctx)
	if err != nil {
		return fmt.Errorf("discovering Delve port: %w", err)
	}
	fmt.Printf("Delve listening at %s\n", addr)

	// Connect to verify the debugger is alive, then disconnect so the
	// TCP connection doesn't go stale when forge exits.
	d := dbgsvc.NewDelveDebugger()
	if err := d.Connect(addr); err != nil {
		return fmt.Errorf("connecting to Delve: %w", err)
	}
	d.Disconnect()

	// Save session.
	session := &dbgsvc.SessionInfo{
		Type:    "delve",
		Addr:    addr,
		Docker:  true,
		Started: time.Now(),
	}
	if err := debugSvc().SaveSession(".", session); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}

	fmt.Println("Docker debug session started")
	return nil
}

func discoverDelvePort(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "compose", "port", "app-debug", "2345").Output()
	if err != nil {
		return "", fmt.Errorf("docker compose port: %w", err)
	}
	addr := strings.TrimSpace(string(out))
	// Normalize 0.0.0.0 to 127.0.0.1
	addr = strings.Replace(addr, "0.0.0.0", "127.0.0.1", 1)
	return addr, nil
}
