// Package cli — `forge cluster` k3d lifecycle subcommands.
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
//   - kubectl context pinning so `forge cluster reload` can't
//     accidentally apply to staging or prod
//   - one-command reload that re-renders KCL + applies + waits for rollout
//     (the inner loop during local dev)
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/config"
)

// defaultK3dConfigPath is the canonical location of the project's k3d
// Simple-config YAML. Override via --config.
const defaultK3dConfigPath = "deploy/k3d.yaml"

// The k3d lifecycle subcommands below (up/down/reset/reload) are
// registered flat under `forge cluster` by newClusterCmd in dev.go.
// `status` is served by the dev_status.go implementation (a superset of
// the old nested cluster status), so there is no newDevClusterStatusCmd.

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
  forge cluster up
  forge cluster up --wait
  forge cluster up --config deploy/k3d.custom.yaml`,
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
docker image (the same code path forge env deploy dev uses, but skips the
cluster bootstrap).

Examples:
  forge cluster reload
  forge cluster reload --image-tag dev-2026-06-01
  forge cluster reload --dry-run`,
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
	ServersCount   int    `json:"serversCount"`
	AgentsRunning  int    `json:"agentsRunning"`
	AgentsCount    int    `json:"agentsCount"`
}

// k3dClusterRuntimeState distinguishes a cluster that merely exists from
// one whose declared server/agent containers are all running. k3d keeps
// stopped clusters in `cluster list`, so presence alone is not sufficient
// for any subsequent kubectl or docker-exec reconciliation.
type k3dClusterRuntimeState struct {
	Exists  bool
	Running bool
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
	if store, err := loadProjectStore(); err == nil {
		return store.Meta().Name, nil
	}
	return "dev", nil
}

// listK3dClusters shells out to `k3d cluster list -o json` and returns
// the parsed list. An empty array (no clusters) returns a nil slice with
// no error.
//
// stdout carries the JSON, so stderr is captured SEPARATELY and folded into
// the error. k3d reports its real diagnosis there (e.g. "runtime failed to
// list nodes: docker failed to get containers ... 500 Internal Server
// Error"); discarding it and printing a blanket "install k3d" hint sent
// callers chasing a missing binary that was installed all along. The
// install hint is now emitted ONLY when the binary is genuinely absent.
func listK3dClusters(ctx context.Context) ([]k3dClusterListEntry, error) {
	cmd := exec.CommandContext(ctx, "k3d", "cluster", "list", "-o", "json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("k3d cluster list: %w (install k3d: https://k3d.io)", err)
		}
		return nil, fmt.Errorf("k3d cluster list: %w%s", err, formatCommandStderr(stderr.String()))
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

// fullyRunning reports whether every k3d server and agent is running. Older
// k3d versions may omit the total-count fields; in that case, at least one
// running server is the strongest available signal and preserves backwards
// compatibility with their JSON shape.
func (e k3dClusterListEntry) fullyRunning() bool {
	if e.ServersRunning == 0 {
		return false
	}
	if e.ServersCount > 0 && e.ServersRunning != e.ServersCount {
		return false
	}
	if e.AgentsCount > 0 && e.AgentsRunning != e.AgentsCount {
		return false
	}
	return true
}

// lookupK3dClusterRuntimeState returns both presence and liveness from one
// `k3d cluster list` snapshot. A stopped cluster exists but is not running.
func lookupK3dClusterRuntimeState(ctx context.Context, name string) (k3dClusterRuntimeState, error) {
	entries, err := listK3dClusters(ctx)
	if err != nil {
		return k3dClusterRuntimeState{}, err
	}
	for _, e := range entries {
		if e.Name == name {
			return k3dClusterRuntimeState{Exists: true, Running: e.fullyRunning()}, nil
		}
	}
	return k3dClusterRuntimeState{}, nil
}

// clusterExists returns true when a cluster of the given name is listed
// by k3d.
func clusterExists(ctx context.Context, name string) (bool, error) {
	state, err := lookupK3dClusterRuntimeState(ctx, name)
	if err != nil {
		return false, err
	}
	return state.Exists, nil
}

const k3dClusterStartTimeout = 90 * time.Second

var (
	runK3dClusterStartCommandFn  = runK3dClusterStartCommand
	lookupK3dStateAfterStartFn   = lookupK3dClusterRuntimeState
	cleanupK3dStartToolsFn       = cleanupK3dStartTools
	runK3dClusterCreateCommandFn = runK3dClusterCreateCommand
	lookupK3dStateAfterCreateFn  = lookupK3dClusterRuntimeState
	cleanupK3dCreateToolsFn      = cleanupK3dStartTools
	mergeK3dKubeconfigFn         = mergeK3dKubeconfig
	k3dClusterStartPollInterval  = time.Second
	k3dClusterStartHealthyGrace  = 10 * time.Second
)

// runK3dClusterStartCommand is split from startK3dCluster so the latter's
// hung-client recovery is unit-testable without launching Docker.
func runK3dClusterStartCommand(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "k3d", "cluster", "start", name,
		"--wait", "--timeout", k3dClusterStartTimeout.String())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runK3dClusterCreateCommand is split from createK3dCluster for the same
// reason as the start command: k3d can wedge in its post-start CoreDNS alias
// injection even though every cluster container is already running.
func runK3dClusterCreateCommand(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "k3d", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// mergeK3dKubeconfig repairs the finalization step skipped when Forge cancels
// a wedged create client after the cluster is already running. It is also safe
// after a normal create: merge is an idempotent update and deliberately does
// not switch the caller's active context.
func mergeK3dKubeconfig(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "k3d", "kubeconfig", "merge", name,
		"--kubeconfig-merge-default", "--kubeconfig-switch-context=false")
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("merge kubeconfig for k3d cluster %q: %w", name, err)
	}
	return nil
}

// cleanupK3dStartTools removes the disposable helper k3d leaves behind when
// its start client is cancelled during host-alias injection. Cluster nodes,
// load balancers, registries, volumes, and networks are untouched.
func cleanupK3dStartTools(ctx context.Context, name string) error {
	container := "k3d-" + name + "-tools"
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", container).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		return fmt.Errorf("remove temporary container %s: %w", container, err)
	}
	return nil
}

func finishK3dClusterLifecycle(ctx, commandCtx context.Context, name, operation string, commandErr error,
	lookup func(context.Context, string) (k3dClusterRuntimeState, error),
	cleanup func(context.Context, string) error,
) error {
	if commandErr == nil {
		if cleanupErr := cleanup(ctx, name); cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "  warning: %v\n", cleanupErr)
		}
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	state, stateErr := lookup(ctx, name)
	if cleanupErr := cleanup(ctx, name); cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "  warning: %v\n", cleanupErr)
	}
	if stateErr == nil && state.Exists && state.Running {
		fmt.Fprintf(os.Stderr,
			"  warning: k3d cluster %s %q returned after the cluster became running (%v) — continuing with readiness checks\n",
			operation, name, commandErr)
		return nil
	}
	if commandCtx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("k3d cluster %s %q timed out after %s", operation, name, k3dClusterStartTimeout)
	}
	if stateErr != nil {
		return fmt.Errorf("k3d cluster %s %q: %w (state recheck also failed: %v)", operation, name, commandErr, stateErr)
	}
	return fmt.Errorf("k3d cluster %s %q: %w", operation, name, commandErr)
}

func runK3dClusterLifecycle(ctx context.Context, name, operation string,
	run func(context.Context) error,
	lookup func(context.Context, string) (k3dClusterRuntimeState, error),
	cleanup func(context.Context, string) error,
) error {
	commandCtx, cancel := context.WithTimeout(ctx, k3dClusterStartTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- run(commandCtx)
	}()

	ticker := time.NewTicker(k3dClusterStartPollInterval)
	defer ticker.Stop()
	var runningSince time.Time

	for {
		select {
		case commandErr := <-done:
			return finishK3dClusterLifecycle(ctx, commandCtx, name, operation, commandErr, lookup, cleanup)
		case <-ticker.C:
			state, err := lookup(ctx, name)
			if err != nil || !state.Exists || !state.Running {
				runningSince = time.Time{}
				continue
			}
			if runningSince.IsZero() {
				runningSince = time.Now()
				continue
			}
			if time.Since(runningSince) < k3dClusterStartHealthyGrace {
				continue
			}

			fmt.Fprintf(os.Stderr,
				"  warning: k3d cluster %q is fully running but its %s client did not return within %s — cancelling the wedged client\n",
				name, operation, k3dClusterStartHealthyGrace)
			cancel()
			commandErr := <-done
			if cleanupErr := cleanup(ctx, name); cleanupErr != nil {
				fmt.Fprintf(os.Stderr, "  warning: %v\n", cleanupErr)
			}
			if commandErr != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		case <-commandCtx.Done():
			commandErr := <-done
			return finishK3dClusterLifecycle(ctx, commandCtx, name, operation, commandErr, lookup, cleanup)
		case <-ctx.Done():
			cancel()
			<-done
			if cleanupErr := cleanup(context.Background(), name); cleanupErr != nil {
				fmt.Fprintf(os.Stderr, "  warning: %v\n", cleanupErr)
			}
			return ctx.Err()
		}
	}
}

// startK3dCluster starts a preserved-but-stopped cluster and waits for k3d's
// servers and load balancer to become ready. k3d v5.8.1 and v5.9.0 can hang
// after the cluster is already running while injecting host aliases into
// CoreDNS. Forge polls authoritative state while k3d runs: once every
// server/agent is up, k3d
// gets a short grace period to finish normally; a still-wedged client is then
// cancelled and its disposable tools helper removed. Callers subsequently
// wait for Kubernetes Nodes and Forge's own DNS/MSS reconciliation.
func startK3dCluster(ctx context.Context, name string) error {
	return runK3dClusterLifecycle(ctx, name, "start",
		func(commandCtx context.Context) error {
			return runK3dClusterStartCommandFn(commandCtx, name)
		},
		lookupK3dStateAfterStartFn,
		cleanupK3dStartToolsFn,
	)
}

// createK3dCluster gives fresh creates the same hung-client recovery as
// starts. k3d 5.9.0 can reach a fully running cluster and then wedge while
// injecting CoreDNS host aliases; authoritative running state plus Forge's
// subsequent readiness/DNS reconciliation is sufficient to continue safely.
func createK3dCluster(ctx context.Context, name string, args []string) error {
	if err := runK3dClusterLifecycle(ctx, name, "create",
		func(commandCtx context.Context) error {
			return runK3dClusterCreateCommandFn(commandCtx, args)
		},
		lookupK3dStateAfterCreateFn,
		cleanupK3dCreateToolsFn,
	); err != nil {
		return err
	}
	return mergeK3dKubeconfigFn(ctx, name)
}

// pinKubectlContext sets the current kubectl context to k3d-<name>.
// k3d names its kubeconfig contexts as `k3d-<cluster-name>` by
// convention, so this is the one-liner guard rail that prevents the
// rest of `forge cluster` from leaking commands into a non-dev context.
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

// resumeExistingDevCluster handles `forge cluster up` against a cluster that
// already exists: start it if stopped, then pin the kubectl context (so the
// kubectl calls that follow cannot land on a stale one) and optionally wait
// for nodes to report Ready. Creation is the caller's job.
func resumeExistingDevCluster(ctx context.Context, clusterName string, state k3dClusterRuntimeState, wait bool) error {
	if state.Running {
		fmt.Printf("k3d cluster %q already exists and is running — no-op\n", clusterName)
	} else {
		fmt.Printf("k3d cluster %q already exists but is stopped — starting...\n", clusterName)
		if err := startK3dCluster(ctx, clusterName); err != nil {
			return err
		}
	}
	if err := pinKubectlContext(ctx, clusterName); err != nil {
		return err
	}
	if wait {
		fmt.Println("Waiting for cluster nodes to report Ready...")
		if err := waitNodeReady(ctx, clusterName); err != nil {
			return fmt.Errorf("wait for cluster nodes: %w", err)
		}
	}
	return nil
}

func runDevClusterUp(ctx context.Context, configPath string, wait bool) error {
	// Deploy-feature gate: `forge cluster up` boots a k3d cluster
	// whose only purpose is hosting the project's deploy. A library /
	// CLI project that's opted out of deploy has no reason to spin
	// one up. Mirrors the deploy gate in runDeploy / runDevClusterReload.
	if store, err := loadProjectStore(); err == nil && !store.Features().DeployEnabled() {
		return config.DisabledFeatureError(config.FeatureDeploy)
	}
	clusterName, err := resolveClusterName(configPath)
	if err != nil {
		return err
	}

	state, err := lookupK3dClusterRuntimeState(ctx, clusterName)
	if err != nil {
		return err
	}
	if state.Exists {
		return resumeExistingDevCluster(ctx, clusterName, state, wait)
	}

	// Resolve the effective k3d config. When features.ingress is on
	// and deploy/k3d-ports.yaml exists, merge the listener-port
	// fragment into the user's deploy/k3d.yaml (in memory) and pass
	// k3d a tempfile holding the merged YAML. See dev_cluster_ingress.go
	// for the merge policy.
	ingressOn := false
	if store, err := loadProjectStore(); err == nil {
		ingressOn = store.Features().IngressEnabled()
	}
	effective, cleanupCfg, err := mergeK3dConfig(configPath, ingressOn)
	if err != nil {
		return err
	}
	defer cleanupCfg()

	// Cluster doesn't exist — create from config if present, else use
	// the same fallback path forge env deploy dev has used historically.
	if _, statErr := os.Stat(effective.path); statErr == nil {
		fmt.Printf("Creating k3d cluster from %s...\n", configPath)
		if effective.temporary {
			fmt.Printf("  (merging deploy/k3d-ports.yaml from project's ingress KCL)\n")
		}
		if err := createK3dCluster(ctx, clusterName,
			[]string{"cluster", "create", "--config", effective.path}); err != nil {
			return err
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

	// The Gateway API stack (Gateway API CRDs + Envoy Gateway controller +
	// the `eg` GatewayClass) is NOT installed imperatively here. It is a
	// DECLARED platform dependency — a forge.HelmChart in the env Bundle's
	// `helm_charts` — rendered + applied by the deploy phase
	// (`forge env deploy <env> --target=envoy-gateway`, CRD-first) EXACTLY like
	// the cloud envs. `forge cluster up` only creates the bare k3d cluster
	// with the host-port mapping the merged k3d config carries; the
	// controller arrives with the deploy. One declarative model everywhere.
	//
	// Still provision mkcert TLS Secrets for any dev Gateway that opted in
	// via tls.mode == "mkcert": those are project-owned Secrets the declared
	// Gateway references, independent of the controller install. No-op when
	// no mkcert gateways are declared (or no dev KCL).
	if ingressOn {
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
	// error symmetric so a `forge cluster up && forge cluster
	// down` cycle fails uniformly when deploy is off.
	if store, err := loadProjectStore(); err == nil && !store.Features().DeployEnabled() {
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
// wait-rollout code path forge env deploy dev uses, with cluster bootstrap
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

	store, err := loadProjectStore()
	if err != nil {
		return err
	}
	if !store.Features().DeployEnabled() {
		return config.DisabledFeatureError(config.FeatureDeploy)
	}

	kclDir := store.K8s().KCLDir
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
			namespace = store.Meta().Name + "-dev"
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
