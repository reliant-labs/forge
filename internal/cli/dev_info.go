// Package cli — `forge dev info` command.
//
// Diagnostic dump of the dev-loop config: which cluster, which context,
// which namespace, which service ports. Replaces the small bash recipe
// every project would otherwise hand-write to debug "why is my
// ingress URL hitting the wrong service?"
package cli

import (
	"context"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
)

func newDevInfoCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Print static dev-loop config (cluster name, expected context, declared ports)",
		Long: `Print the static dev-loop config declared in forge.yaml + deploy/k3d.yaml.

Static means "what the project says it expects" — cluster name, expected
kubectl context, registry URL, declared service/frontend ports. It does
NOT contact the cluster or check pod state.

For dynamic state (is the cluster up? are pods running? what are the
live ingress URLs?) use ` + "`forge dev status`" + `.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDevInfo(configPath)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", defaultK3dConfigPath, "k3d config file")
	return cmd
}

func runDevInfo(configPath string) error {
	clusterName, err := resolveClusterName(configPath)
	if err != nil {
		return err
	}
	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}
	ns := devNamespace(clusterName)
	expectedCtx := "k3d-" + clusterName

	registry := "localhost:5050"
	if reg := k8sClusterRegistryForEnv(context.Background(), "dev"); reg != "" {
		registry = reg
	}

	fmt.Printf("Project:                    %s\n", cfg.Name)
	fmt.Printf("Cluster (declared):         %s\n", clusterName)
	fmt.Printf("Namespace (declared):       %s\n", ns)
	fmt.Printf("Registry (declared):        %s\n", registry)
	fmt.Printf("kubectl context (expected): %s\n", expectedCtx)
	fmt.Printf("k3d config:                 %s\n", configPath)
	fmt.Println()
	fmt.Println("Declared component ports:")
	printServicePorts(cfg.Components)
	if len(cfg.Frontends) > 0 {
		fmt.Println()
		fmt.Println("Declared frontend ports:")
		printFrontendPorts(cfg.Frontends)
	}
	fmt.Println()
	fmt.Println("For dynamic state (cluster up/down, pods, ingress URLs), run `forge dev status`.")
	return nil
}

func printServicePorts(comps []config.ComponentConfig) {
	if len(comps) == 0 {
		fmt.Println("  (none)")
		return
	}
	sorted := append([]config.ComponentConfig{}, comps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, c := range sorted {
		port := c.PrimaryPort()
		if port == 0 {
			fmt.Printf("  %-30s (no port declared)\n", c.Name)
			continue
		}
		fmt.Printf("  %-30s %d\n", c.Name, port)
	}
}

func printFrontendPorts(fes []config.FrontendConfig) {
	sorted := append([]config.FrontendConfig{}, fes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, f := range sorted {
		port := f.Port
		if port == 0 {
			fmt.Printf("  %-30s (no port declared)\n", f.Name)
			continue
		}
		fmt.Printf("  %-30s %d\n", f.Name, port)
	}
}
