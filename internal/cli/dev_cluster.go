// Package cli — `forge dev cluster` subtree.
//
// This file consolidates the k3d cluster lifecycle that every k8s-targeting
// forge project would otherwise hand-write in bash (~30-50 lines of
// idempotent create / delete / wait-for-rollout / context-pin logic).
//
// k3d itself is the source of truth for cluster state — we shell out to
// `k3d cluster create/delete/list` rather than reinvent. The value forge
// adds is:
//
//   - read deploy/k3d.yaml as the canonical config (no hand-written
//     --servers/--no-lb/--registry-create flags scattered across scripts)
//   - idempotent up/down semantics (`up` no-ops if the cluster exists,
//     `down` no-ops if it doesn't)
//   - kubectl context pinning so `forge dev cluster reload` can't
//     accidentally apply to staging or prod
//   - one-command reload that re-renders KCL + applies + waits for rollout
//     (the inner loop during local dev)
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/config"
)

// defaultK3dConfigPath is the canonical location of the project's k3d
// Simple-config YAML. Override via --config.
const defaultK3dConfigPath = "deploy/k3d.yaml"

// newDevClusterCmd builds the `forge dev cluster` subtree.
func newDevClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Manage the local k3d cluster",
		Long: `Manage the local k3d development cluster.

The cluster config is read from deploy/k3d.yaml (override via --config).
All subcommands pin the kubectl context to k3d-<cluster-name> as a
guardrail against accidental prod-context leaks.`,
	}

	cmd.AddCommand(newDevClusterUpCmd())
	cmd.AddCommand(newDevClusterDownCmd())
	cmd.AddCommand(newDevClusterStatusCmd())
	cmd.AddCommand(newDevClusterResetCmd())
	cmd.AddCommand(newDevClusterReloadCmd())

	return cmd
}

func newDevClusterUpCmd() *cobra.Command {
	var (
		configPath string
		wait       bool
	)
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create the k3d cluster from deploy/k3d.yaml",
		Long: `Create the k3d cluster from deploy/k3d.yaml.

If the cluster already exists, this is a no-op success. With --wait,
blocks until the cluster's nodes report ready.

Examples:
  forge dev cluster up
  forge dev cluster up --wait
  forge dev cluster up --config deploy/k3d.custom.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDevClusterUp(cmd.Context(), configPath, wait)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", defaultK3dConfigPath, "k3d config file")
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait until cluster nodes are ready")
	return cmd
}

func newDevClusterDownCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Delete the k3d cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDevClusterDown(cmd.Context(), configPath)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", defaultK3dConfigPath, "k3d config file")
	return cmd
}

func newDevClusterStatusCmd() *cobra.Command {
	var (
		configPath string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show cluster up/down state, registry, and API port",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDevClusterStatus(cmd.Context(), configPath, jsonOut)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", defaultK3dConfigPath, "k3d config file")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newDevClusterResetCmd() *cobra.Command {
	var (
		configPath string
		wait       bool
	)
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Delete then recreate the cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runDevClusterDown(cmd.Context(), configPath); err != nil {
				return err
			}
			return runDevClusterUp(cmd.Context(), configPath, wait)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", defaultK3dConfigPath, "k3d config file")
	cmd.Flags().BoolVar(&wait, "wait", true, "Wait until cluster nodes are ready after recreate")
	return cmd
}

func newDevClusterReloadCmd() *cobra.Command {
	var (
		configPath string
		imageTag   string
		namespace  string
		dryRun     bool
	)
	cmd := &cobra.Command{
		Use:   "reload",
		Short: "Re-render deploy/kcl/dev + kubectl apply + wait rollout",
		Long: `Re-render the dev KCL manifests, apply, and wait for rollout.

This is the inner loop during local development: after editing code or
KCL, run this to push the change into the cluster without rebuilding the
docker image (the same code path forge deploy dev uses, but skips the
cluster bootstrap).

Examples:
  forge dev cluster reload
  forge dev cluster reload --image-tag dev-2026-06-01
  forge dev cluster reload --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDevClusterReload(cmd.Context(), configPath, imageTag, namespace, dryRun)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", defaultK3dConfigPath, "k3d config file")
	cmd.Flags().StringVar(&imageTag, "image-tag", "", "Image tag (default: git short SHA)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Override namespace from environment config")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print manifests without applying")
	return cmd
}

// k3dSimpleConfig is the subset of the k3d "simple config" YAML we
// inspect. The full schema is large; we only need the cluster name to
// resolve `k3d-<name>` for the kubectl context.
type k3dSimpleConfig struct {
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
}

// k3dClusterListEntry mirrors the `k3d cluster list -o json` element
// shape — only the fields we read.
type k3dClusterListEntry struct {
	Name           string `json:"name"`
	ServersRunning int    `json:"serversRunning"`
	AgentsRunning  int    `json:"agentsRunning"`
}

// readK3dClusterName parses the k3d config file and returns the cluster
// name. Falls back to "" when the file is missing (callers pass the
// fallback name explicitly).
func readK3dClusterName(configPath string) (string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", configPath, err)
	}
	var cfg k3dSimpleConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse %s: %w", configPath, err)
	}
	return cfg.Metadata.Name, nil
}

// resolveClusterName returns the cluster name to operate on. Priority:
// 1) name in deploy/k3d.yaml metadata.name
// 2) project name from forge.yaml
// 3) "dev" (last-resort default matching forge's previous behavior)
func resolveClusterName(configPath string) (string, error) {
	if name, err := readK3dClusterName(configPath); err == nil && name != "" {
		return name, nil
	}
	if cfg, err := loadProjectConfig(); err == nil {
		return cfg.Name, nil
	}
	return "dev", nil
}

// listK3dClusters shells out to `k3d cluster list -o json` and returns
// the parsed list. An empty array (no clusters) returns a nil slice with
// no error.
func listK3dClusters(ctx context.Context) ([]k3dClusterListEntry, error) {
	out, err := exec.CommandContext(ctx, "k3d", "cluster", "list", "-o", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("k3d cluster list: %w (install k3d: https://k3d.io)", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[]" {
		return nil, nil
	}
	var entries []k3dClusterListEntry
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return nil, fmt.Errorf("parse k3d cluster list output: %w", err)
	}
	return entries, nil
}

// clusterExists returns true when a cluster of the given name is listed
// by k3d.
func clusterExists(ctx context.Context, name string) (bool, error) {
	entries, err := listK3dClusters(ctx)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// pinKubectlContext sets the current kubectl context to k3d-<name>.
// k3d names its kubeconfig contexts as `k3d-<cluster-name>` by
// convention, so this is the one-liner guard rail that prevents the
// rest of `forge dev` from leaking commands into a non-dev context.
func pinKubectlContext(ctx context.Context, clusterName string) error {
	target := "k3d-" + clusterName
	cmd := exec.CommandContext(ctx, "kubectl", "config", "use-context", target)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl config use-context %s: %w", target, err)
	}
	return nil
}

// currentKubectlContext returns the current kubectl context name, or ""
// on error. Used for non-fatal display in `status` / `info`.
func currentKubectlContext(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "kubectl", "config", "current-context").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func runDevClusterUp(ctx context.Context, configPath string, wait bool) error {
	// Deploy-feature gate: `forge dev cluster up` boots a k3d cluster
	// whose only purpose is hosting the project's deploy. A library /
	// CLI project that's opted out of deploy has no reason to spin
	// one up. Mirrors the deploy gate in runDeploy / runDevClusterReload.
	if cfg, err := loadProjectConfig(); err == nil && !cfg.Features.DeployEnabled() {
		return config.DisabledFeatureError(config.FeatureDeploy)
	}
	clusterName, err := resolveClusterName(configPath)
	if err != nil {
		return err
	}

	exists, err := clusterExists(ctx, clusterName)
	if err != nil {
		return err
	}
	if exists {
		fmt.Printf("k3d cluster %q already exists — no-op\n", clusterName)
		// Still pin context so subsequent kubectl calls are safe.
		if err := pinKubectlContext(ctx, clusterName); err != nil {
			return err
		}
		return nil
	}

	// Resolve the effective k3d config. When features.ingress is on
	// and deploy/k3d-ports.yaml exists, merge the listener-port
	// fragment into the user's deploy/k3d.yaml (in memory) and pass
	// k3d a tempfile holding the merged YAML. See dev_cluster_ingress.go
	// for the merge policy.
	ingressOn := false
	if cfg, err := loadProjectConfig(); err == nil {
		ingressOn = cfg.Features.IngressEnabled()
	}
	effective, cleanupCfg, err := mergeK3dConfig(configPath, ingressOn)
	if err != nil {
		return err
	}
	defer cleanupCfg()

	// Cluster doesn't exist — create from config if present, else use
	// the same fallback path forge deploy dev has used historically.
	if _, statErr := os.Stat(effective.path); statErr == nil {
		fmt.Printf("Creating k3d cluster from %s...\n", configPath)
		if effective.temporary {
			fmt.Printf("  (merging deploy/k3d-ports.yaml from project's ingress KCL)\n")
		}
		create := exec.CommandContext(ctx, "k3d", "cluster", "create", "--config", effective.path)
		create.Stdout = os.Stdout
		create.Stderr = os.Stderr
		if err := create.Run(); err != nil {
			return fmt.Errorf("k3d cluster create: %w", err)
		}
	} else {
		// Reuse the existing ensureDevCluster path so the fallback
		// registries.yaml mirror config stays in one place.
		fmt.Printf("No %s found — falling back to forge default cluster shape...\n", configPath)
		if err := ensureDevCluster(ctx); err != nil {
			return err
		}
	}

	if err := pinKubectlContext(ctx, clusterName); err != nil {
		return err
	}

	if wait {
		fmt.Println("Waiting for cluster nodes to report Ready...")
		waitCmd := exec.CommandContext(ctx, "kubectl", "wait", "--for=condition=Ready",
			"nodes", "--all", "--timeout=120s")
		waitCmd.Stdout = os.Stdout
		waitCmd.Stderr = os.Stderr
		if err := waitCmd.Run(); err != nil {
			return fmt.Errorf("wait for cluster nodes: %w", err)
		}
	}

	// Install the ingress bundle (Gateway API CRDs + Traefik +
	// GatewayClass) when the project has features.ingress enabled.
	// Runs AFTER the kubectl context is pinned so applies hit the
	// right cluster.
	if ingressOn {
		if err := installIngressBundle(ctx); err != nil {
			return fmt.Errorf("install ingress: %w", err)
		}
		// Provision mkcert TLS Secrets for any dev Gateway that
		// opted in via tls.mode == "mkcert". Runs AFTER the ingress
		// bundle so the GatewayClass is ready when the Secret lands;
		// no-op when no mkcert gateways are declared.
		projectDir, _ := os.Getwd()
		if err := provisionMkcertSecrets(ctx, projectDir); err != nil {
			return fmt.Errorf("provision mkcert TLS: %w", err)
		}
	}

	fmt.Printf("k3d cluster %q is up.\n", clusterName)
	return nil
}

func runDevClusterDown(ctx context.Context, configPath string) error {
	// Same gate as runDevClusterUp — tearing down a cluster that
	// `cluster up` won't create is at worst a no-op, but we keep the
	// error symmetric so a `forge dev cluster up && forge dev cluster
	// down` cycle fails uniformly when deploy is off.
	if cfg, err := loadProjectConfig(); err == nil && !cfg.Features.DeployEnabled() {
		return config.DisabledFeatureError(config.FeatureDeploy)
	}
	clusterName, err := resolveClusterName(configPath)
	if err != nil {
		return err
	}

	exists, err := clusterExists(ctx, clusterName)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Printf("k3d cluster %q not found — no-op\n", clusterName)
		return nil
	}

	fmt.Printf("Deleting k3d cluster %q...\n", clusterName)
	del := exec.CommandContext(ctx, "k3d", "cluster", "delete", clusterName)
	del.Stdout = os.Stdout
	del.Stderr = os.Stderr
	if err := del.Run(); err != nil {
		return fmt.Errorf("k3d cluster delete: %w", err)
	}
	return nil
}

// clusterStatusSummary is the data shape rendered by `cluster status`.
// Used for both human and --json output.
type clusterStatusSummary struct {
	Name        string `json:"name"`
	Exists      bool   `json:"exists"`
	Context     string `json:"kubectl_context"`
	Registry    string `json:"registry,omitempty"`
	APIPort     string `json:"api_port,omitempty"`
	ConfigPath  string `json:"config_path"`
	ConfigFound bool   `json:"config_found"`
}

func runDevClusterStatus(ctx context.Context, configPath string, jsonOut bool) error {
	clusterName, err := resolveClusterName(configPath)
	if err != nil {
		return err
	}

	exists, _ := clusterExists(ctx, clusterName)
	_, statErr := os.Stat(configPath)

	summary := clusterStatusSummary{
		Name:        clusterName,
		Exists:      exists,
		Context:     "k3d-" + clusterName,
		ConfigPath:  configPath,
		ConfigFound: statErr == nil,
	}

	// Pull registry/port from the k3d cluster list entry when up.
	if exists {
		if entries, err := listK3dClusters(ctx); err == nil {
			for _, e := range entries {
				if e.Name == clusterName {
					// k3d exposes ports via the cluster info; we
					// surface what's available without parsing
					// the full k3d cluster info schema. Leave
					// these blank when not trivially derivable.
					_ = e
					break
				}
			}
		}
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(summary)
	}

	fmt.Printf("Cluster:         %s\n", summary.Name)
	fmt.Printf("State:           %s\n", boolUpDown(summary.Exists))
	fmt.Printf("kubectl context: %s\n", summary.Context)
	fmt.Printf("Config:          %s (%s)\n", summary.ConfigPath, foundOrMissing(summary.ConfigFound))
	return nil
}

func boolUpDown(b bool) string {
	if b {
		return "up"
	}
	return "down"
}

func foundOrMissing(b bool) string {
	if b {
		return "found"
	}
	return "missing"
}

// runDevClusterReload invokes the same KCL render + kubectl apply +
// wait-rollout code path forge deploy dev uses, with cluster bootstrap
// and docker build/push skipped. Pins kubectl context first so a stale
// non-dev context can't leak the apply somewhere unintended.
func runDevClusterReload(ctx context.Context, configPath, imageTag, namespace string, dryRun bool) error {
	clusterName, err := resolveClusterName(configPath)
	if err != nil {
		return err
	}

	if !dryRun {
		if err := pinKubectlContext(ctx, clusterName); err != nil {
			return err
		}
	}

	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}
	if !cfg.Features.DeployEnabled() {
		return config.DisabledFeatureError(config.FeatureDeploy)
	}

	kclDir := cfg.K8s.KCLDir
	if kclDir == "" {
		kclDir = "deploy/kcl"
	}
	envDir := filepath.Join(kclDir, "dev")
	mainK := filepath.Join(envDir, "main.k")
	if _, err := os.Stat(mainK); os.IsNotExist(err) {
		return fmt.Errorf("dev KCL not found: %s does not exist (run forge generate first?)", mainK)
	}

	if imageTag == "" {
		// Reload assumes the image is already in the cluster's
		// registry; default to the most recent SHA we have.
		tag, err := gitShortSHA(ctx)
		if err != nil {
			return fmt.Errorf("git rev-parse --short HEAD: %w (use --image-tag)", err)
		}
		imageTag = tag
	}

	if namespace == "" {
		if ns := k8sClusterNamespaceForEnv(ctx, "dev"); ns != "" {
			namespace = ns
		} else {
			namespace = cfg.Name + "-dev"
		}
	}

	fmt.Printf("Reloading dev manifests for cluster %q (namespace=%s, tag=%s)...\n",
		clusterName, namespace, imageTag)

	// Reload deliberately skips the deploy-time extras: no per-env
	// config projection (rebuilds defeat the inner-loop purpose), no
	// prune, no host-skip filter, no one-shot Job wait. Quiet=true
	// suppresses the section-header banners and matches the shorter
	// error wraps the pre-extraction reload used. The dry-run output
	// is unframed (raw manifests).
	return cluster.Apply(ctx, cluster.ApplyOpts{
		MainK:     mainK,
		ImageTag:  imageTag,
		Namespace: namespace,
		Env:       "dev", // dev cluster reload is dev-only (matches the envDir above)
		DryRun:    dryRun,
		Quiet:     true,
	})
}
