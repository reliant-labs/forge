// Package cli — `forge dev status` command.
//
// Renders a human- or machine-readable snapshot of the dev cluster,
// running pods, and active port-forwards. Replaces a 30-line bash recipe
// every k8s-targeting forge project would otherwise hand-write.
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

func newDevStatusCmd() *cobra.Command {
	var (
		configPath string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print dynamic dev-loop state (cluster up/down, pods, port-forwards)",
		Long: `Print the dynamic state of the local dev environment.

Dynamic means "what's actually happening right now" — does the k3d
cluster exist, what's the current kubectl context, what pods are in
the dev namespace, what port-forwards are running, what sibling dev
namespaces exist on this cluster.

For static config (declared cluster name, expected context, declared
service/frontend ports) run ` + "`forge dev info`" + `.

Examples:
  forge dev status
  forge dev status --json    # machine-readable for scripts/dashboards`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDevStatus(configPath, jsonOut)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", defaultK3dConfigPath, "k3d config file")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

// devStatusSummary is the rendered shape — kept stable so --json output
// is consumable by dashboards and scripts.
type devStatusSummary struct {
	Cluster       clusterStatusSummary `json:"cluster"`
	Namespace     string               `json:"namespace"`
	Pods          []podStatusEntry     `json:"pods"`
	PortForwards  []portForwardEntry   `json:"port_forwards"`
	Siblings      []string             `json:"siblings"`
}

type podStatusEntry struct {
	Name    string `json:"name"`
	Ready   string `json:"ready"`    // e.g. "1/1"
	Status  string `json:"status"`   // e.g. "Running"
	Restart string `json:"restarts"` // e.g. "0"
	Age     string `json:"age"`
}

func runDevStatus(configPath string, jsonOut bool) error {
	clusterName, err := resolveClusterName(configPath)
	if err != nil {
		return err
	}

	exists, _ := clusterExists(clusterName)
	_, statErr := os.Stat(configPath)

	cluster := clusterStatusSummary{
		Name:        clusterName,
		Exists:      exists,
		Context:     "k3d-" + clusterName,
		ConfigPath:  configPath,
		ConfigFound: statErr == nil,
	}

	ns := devNamespace(clusterName)
	summary := devStatusSummary{
		Cluster:   cluster,
		Namespace: ns,
	}

	if exists {
		summary.Pods = listPodsInNamespace(ns)
		summary.PortForwards = readPortForwardState(clusterName, ns)
		summary.Siblings = listSiblingNamespaces(clusterName, ns)
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(summary)
	}

	// Dynamic state: is the cluster up, what's the current kubectl
	// context, what namespace are we reading from. Declared values
	// (expected cluster name, expected context) live in `forge dev info`.
	fmt.Printf("Cluster %s: %s\n", cluster.Name, boolUpDown(cluster.Exists))
	current := currentKubectlContext()
	if current == "" {
		fmt.Printf("kubectl context (current): (none)\n")
	} else if current == cluster.Context {
		fmt.Printf("kubectl context (current): %s (matches expected)\n", current)
	} else {
		fmt.Printf("kubectl context (current): %s (expected %s — run `kubectl config use-context %s`)\n",
			current, cluster.Context, cluster.Context)
	}
	fmt.Printf("Namespace: %s\n", ns)
	fmt.Println()
	if !exists {
		fmt.Println("Cluster is down — run `forge dev cluster up` to start.")
		fmt.Println("Run `forge dev info` for the declared config.")
		return nil
	}

	fmt.Println("Pods:")
	if len(summary.Pods) == 0 {
		fmt.Println("  (none)")
	} else {
		fmt.Printf("  %-40s %-8s %-12s %-10s %s\n", "NAME", "READY", "STATUS", "RESTARTS", "AGE")
		for _, p := range summary.Pods {
			fmt.Printf("  %-40s %-8s %-12s %-10s %s\n", p.Name, p.Ready, p.Status, p.Restart, p.Age)
		}
	}

	fmt.Println()
	fmt.Println("Port-forwards:")
	if len(summary.PortForwards) == 0 {
		fmt.Println("  (none — run `forge dev port-forward` to start)")
	} else {
		fmt.Printf("  %-30s %-8s %s\n", "SERVICE", "PID", "PORT")
		for _, pf := range summary.PortForwards {
			fmt.Printf("  %-30s %-8d %d:%d\n", pf.Service, pf.PID, pf.LocalPort, pf.RemotePort)
		}
	}

	if len(summary.Siblings) > 0 {
		fmt.Println()
		fmt.Println("Sibling dev namespaces on this cluster:")
		for _, s := range summary.Siblings {
			fmt.Printf("  - %s\n", s)
		}
	}
	fmt.Println()
	fmt.Println("For declared port mappings, run `forge dev info`.")
	return nil
}

// devNamespace resolves the namespace forge dev operates against. Reads
// the dev environment's namespace from forge.yaml when present; falls
// back to <project>-dev (which matches forge deploy dev's behavior).
func devNamespace(clusterName string) string {
	cfg, err := loadProjectConfig()
	if err != nil {
		return clusterName + "-dev"
	}
	if env := findEnvironment(cfg, "dev"); env != nil && env.Namespace != "" {
		return env.Namespace
	}
	return cfg.Name + "-dev"
}

// listPodsInNamespace returns a compact pod table for the given
// namespace. Failures are non-fatal — we return an empty slice and let
// the caller render "(none)".
func listPodsInNamespace(ns string) []podStatusEntry {
	cmd := exec.Command("kubectl", "get", "pods", "-n", ns, "--no-headers",
		"-o", "custom-columns=NAME:.metadata.name,READY:.status.containerStatuses[*].ready,STATUS:.status.phase,RESTARTS:.status.containerStatuses[*].restartCount,AGE:.metadata.creationTimestamp")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var entries []podStatusEntry
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		entries = append(entries, podStatusEntry{
			Name:    fields[0],
			Ready:   fields[1],
			Status:  fields[2],
			Restart: fields[3],
			Age:     fields[4],
		})
	}
	return entries
}

// listSiblingNamespaces returns all forge-managed namespaces on the
// cluster other than the project's primary dev namespace. Used to
// surface the multi-worktree workflow (each worktree gets its own
// namespace via the env override).
func listSiblingNamespaces(clusterName, primary string) []string {
	cmd := exec.Command("kubectl", "get", "namespaces",
		"-l", "app.kubernetes.io/managed-by=forge",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var siblings []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == primary {
			continue
		}
		siblings = append(siblings, line)
	}
	return siblings
}
