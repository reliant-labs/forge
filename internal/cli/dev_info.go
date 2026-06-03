// Package cli — `forge dev info` command.
//
// Diagnostic dump of the dev-loop config: which cluster, which context,
// which namespace, which service ports. Replaces the small bash recipe
// every project would otherwise hand-write to debug "why is my
// port-forward going to the wrong place?"
package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
)

func newDevInfoCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Dump dev-loop config: cluster, context, namespace, port mappings",
		Long: `Diagnostic dump of the dev-loop config.

Prints the canonical values forge dev uses so you can sanity-check
mismatches between forge.yaml, deploy/k3d.yaml, and your kubectl
context before diving into port-forward / logs / reload.`,
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
	currentCtx := currentKubectlContext()
	expectedCtx := "k3d-" + clusterName

	registry := "localhost:5050"
	if env := findEnvironment(cfg, "dev"); env != nil && env.Registry != "" {
		registry = env.Registry
	}

	fmt.Printf("Project:                 %s\n", cfg.Name)
	fmt.Printf("Cluster:                 %s\n", clusterName)
	fmt.Printf("Namespace:               %s\n", ns)
	fmt.Printf("Registry:                %s\n", registry)
	fmt.Printf("kubectl context (expected): %s\n", expectedCtx)
	fmt.Printf("kubectl context (current):  %s\n", currentCtx)
	if currentCtx != "" && currentCtx != expectedCtx {
		fmt.Println("WARNING: current context does not match expected — run `forge dev cluster up` or `kubectl config use-context` to fix.")
	}
	fmt.Printf("k3d config:              %s\n", configPath)
	fmt.Println()
	fmt.Println("Declared service ports:")
	printServicePorts(cfg.Services)
	if len(cfg.Frontends) > 0 {
		fmt.Println()
		fmt.Println("Declared frontend ports:")
		printFrontendPorts(cfg.Frontends)
	}
	return nil
}

func printServicePorts(svcs []config.ServiceConfig) {
	if len(svcs) == 0 {
		fmt.Println("  (none)")
		return
	}
	sorted := append([]config.ServiceConfig{}, svcs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, s := range sorted {
		port := s.Port
		if port == 0 {
			fmt.Printf("  %-30s (no port declared)\n", s.Name)
			continue
		}
		fmt.Printf("  %-30s %d\n", s.Name, port)
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
