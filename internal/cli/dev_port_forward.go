// Package cli — `forge dev port-forward` command.
//
// Reads forge.yaml's service list, kicks off `kubectl port-forward` for
// each declared service in parallel, and tracks PIDs in a per-namespace
// state file so `forge dev status` can show them and Ctrl-C cleans them
// up cleanly.
//
// Replaces ~40 lines of bash every k8s-targeting forge project would
// otherwise hand-write (loop over services, kubectl pf, trap SIGINT,
// wait, kill).
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func newDevPortForwardCmd() *cobra.Command {
	var (
		configPath string
		background bool
	)
	cmd := &cobra.Command{
		Use:   "port-forward",
		Short: "Forward every declared service port; Ctrl-C cleans up",
		Long: `Forward every service port declared in forge.yaml.

For each service with a Port field, runs kubectl port-forward in
parallel against the matching Deployment in the dev namespace. PIDs
are written to $HOME/.cache/forge/dev/<cluster>/<ns>.pids so other
forge dev commands can see what's running. Ctrl-C kills every forward.

With --background, the forwards detach from the current shell and the
command returns immediately. Use ` + "`forge dev port-forward stop`" + ` to tear
them down. Useful for orchestration scripts (cloud-dev.sh, CI smoke
tests) that need port-forwards running alongside other steps.

Examples:
  forge dev port-forward
  forge dev port-forward --background
  forge dev port-forward stop
  forge dev port-forward --config deploy/k3d.custom.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDevPortForward(configPath, background)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", defaultK3dConfigPath, "k3d config file")
	cmd.Flags().BoolVar(&background, "background", false, "Run port-forwards detached and return immediately (stop with `forge dev port-forward stop`)")
	cmd.AddCommand(newDevPortForwardStopCmd())
	return cmd
}

// newDevPortForwardStopCmd reads the PID file written by --background
// (or a still-running foreground invocation) and terminates every
// listed kubectl port-forward process.
func newDevPortForwardStopCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop background port-forwards started by `forge dev port-forward --background`",
		Long: `Stop background port-forwards.

Reads $HOME/.cache/forge/dev/<cluster>/<ns>.pids and sends SIGTERM
to each tracked PID. Removes the state file on success. Safe to run
when no forwards are active — it just prints "(none)" and returns 0.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDevPortForwardStop(configPath)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", defaultK3dConfigPath, "k3d config file")
	return cmd
}

// portForwardEntry is a single tracked forward — written to the
// per-namespace state file as JSON Lines.
type portForwardEntry struct {
	Service    string `json:"service"`
	PID        int    `json:"pid"`
	LocalPort  int    `json:"local_port"`
	RemotePort int    `json:"remote_port"`
	StartedAt  string `json:"started_at"`
}

func runDevPortForward(configPath string, background bool) error {
	clusterName, err := resolveClusterName(configPath)
	if err != nil {
		return err
	}
	ns := devNamespace(clusterName)

	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}

	if err := pinKubectlContext(clusterName); err != nil {
		return err
	}

	// Collect services with declared ports + frontends with declared ports.
	type target struct {
		name string
		port int
	}
	var targets []target
	for _, s := range cfg.Services {
		if s.Port > 0 {
			targets = append(targets, target{name: s.Name, port: s.Port})
		}
	}
	for _, f := range cfg.Frontends {
		if f.Port > 0 {
			targets = append(targets, target{name: f.Name, port: f.Port})
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("no services with declared ports in forge.yaml")
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })

	statePath, err := portForwardStatePath(clusterName, ns)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	var (
		mu      sync.Mutex
		entries []portForwardEntry
		procs   []*exec.Cmd
	)

	fmt.Printf("Port-forwarding %d targets in namespace %s...\n", len(targets), ns)
	for _, t := range targets {
		// kubectl will resolve the deployment by name; if a custom
		// scheme uses <project>-<svc>, fall back to the service
		// resource (kubectl port-forward accepts either).
		ref := "deployment/" + t.name
		args := []string{"port-forward", "-n", ns, ref,
			fmt.Sprintf("%d:%d", t.port, t.port)}
		cmd := exec.Command("kubectl", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Printf("  %s: failed to start port-forward: %v\n", t.name, err)
			continue
		}
		fmt.Printf("  %s: kubectl port-forward %s %d:%d (pid=%d)\n",
			t.name, ref, t.port, t.port, cmd.Process.Pid)
		mu.Lock()
		procs = append(procs, cmd)
		entries = append(entries, portForwardEntry{
			Service:    t.name,
			PID:        cmd.Process.Pid,
			LocalPort:  t.port,
			RemotePort: t.port,
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
		})
		mu.Unlock()
	}

	if err := writePortForwardState(statePath, entries); err != nil {
		fmt.Printf("Warning: write state file %s: %v\n", statePath, err)
	}

	// Background mode: detach from the spawned processes and return.
	// kubectl port-forward inherits stdout/stderr but we explicitly
	// don't Wait() — Go's child processes survive parent exit on
	// posix systems, and the PID file is the handle for stop.
	if background {
		fmt.Printf("\n%d port-forward(s) running in background. Stop with `forge dev port-forward stop`.\n", len(procs))
		fmt.Printf("State file: %s\n", statePath)
		// Release process handles so Go's runtime doesn't reap on exit.
		for _, p := range procs {
			if p.Process != nil {
				_ = p.Process.Release()
			}
		}
		return nil
	}

	// Wait for SIGINT/SIGTERM, then kill every forward.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	fmt.Println("\nStopping port-forwards...")
	for _, p := range procs {
		if p.Process != nil {
			_ = p.Process.Signal(syscall.SIGTERM)
		}
	}
	for _, p := range procs {
		_ = p.Wait()
	}
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("Warning: remove state file %s: %v\n", statePath, err)
	}
	return nil
}

// runDevPortForwardStop reads the per-namespace PID file and sends
// SIGTERM to every tracked process, then removes the state file. Each
// kill is best-effort: a missing process (already-dead, restarted host)
// is logged but doesn't abort the rest. Returns nil for the "no
// forwards running" case so orchestration scripts can call stop
// unconditionally on teardown.
func runDevPortForwardStop(configPath string) error {
	clusterName, err := resolveClusterName(configPath)
	if err != nil {
		return err
	}
	ns := devNamespace(clusterName)
	entries := readPortForwardState(clusterName, ns)
	if len(entries) == 0 {
		fmt.Println("No background port-forwards tracked. (none)")
		return nil
	}
	fmt.Printf("Stopping %d port-forward(s)...\n", len(entries))
	for _, e := range entries {
		proc, ferr := os.FindProcess(e.PID)
		if ferr != nil {
			fmt.Printf("  %s (pid=%d): find: %v\n", e.Service, e.PID, ferr)
			continue
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			fmt.Printf("  %s (pid=%d): SIGTERM: %v\n", e.Service, e.PID, err)
			continue
		}
		fmt.Printf("  %s (pid=%d): stopped\n", e.Service, e.PID)
	}
	statePath, err := portForwardStatePath(clusterName, ns)
	if err == nil {
		if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
			fmt.Printf("Warning: remove state file %s: %v\n", statePath, err)
		}
	}
	return nil
}

// portForwardStatePath returns the canonical state file path:
//
//	$HOME/.cache/forge/dev/<cluster>/<namespace>.pids
//
// Same cache prefix every forge subcommand uses; the .pids extension
// makes the file's purpose obvious.
func portForwardStatePath(clusterName, namespace string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".cache", "forge", "dev", clusterName, namespace+".pids"), nil
}

// writePortForwardState writes one JSON object per line — easy to grep
// and easy to consume from `forge dev status`.
func writePortForwardState(path string, entries []portForwardEntry) error {
	var sb strings.Builder
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(sb.String()), 0644)
}

// readPortForwardState reads the per-namespace PID file written by
// `forge dev port-forward` and returns the entries. Missing file or
// parse failures return nil — non-fatal, the caller renders "(none)".
func readPortForwardState(clusterName, namespace string) []portForwardEntry {
	path, err := portForwardStatePath(clusterName, namespace)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []portForwardEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e portForwardEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries
}
