// Package cli — cross-cluster kubeconfig minting (forge.KubeconfigSecret).
//
// In a multi-k3d-cluster dev/e2e env, a workload in cluster A may need to
// talk to cluster B's API server. A kubeconfig with B's serverlb IP baked
// in goes stale every time B is recreated (k3d hands the container a fresh
// docker-network IP). This file mints that kubeconfig FRESH each `forge
// up` — resolving the endpoint at mint time, never persisting the IP —
// and stores it as a k8s Secret the workload mounts.
//
// The "in-network" reachability seam is the dev/e2e path: the target's
// API server is only reachable by its serverlb container IP on the shared
// docker network, and that IP isn't in the serverlb cert SANs, so the
// minted kubeconfig points at https://<ip>:6443 with TLS verification
// disabled. "endpoint" (prod) uses the kubeconfig's own endpoint verbatim
// — a stable reachable address with a valid cert — and needs none of the
// insecure rewrite.
package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/reliant-labs/forge/internal/cluster"
)

// mintKubeconfigSecrets mints every declared cross-cluster kubeconfig and
// applies it as a Secret. Runs at the cluster→deploy boundary — AFTER the
// clusters exist (so `k3d kubeconfig get` + the serverlb container are
// available) and BEFORE workloads mount the Secret. A nil/empty list is a
// no-op.
//
// `ownerNetwork` is the docker network the in-network IP is resolved on —
// the network the clusters share (k3d names a cluster's auto-created
// network `k3d-<owner>`). Derived by the caller from the env's declared
// clusters.
func mintKubeconfigSecrets(ctx context.Context, secrets []KubeconfigSecretEntity, ownerNetwork, defaultNamespace string) error {
	if len(secrets) == 0 {
		return nil
	}
	fmt.Printf("\n[up] kubeconfig phase — minting %d cross-cluster kubeconfig(s)\n", len(secrets))
	for i := range secrets {
		if err := mintOneKubeconfigSecret(ctx, secrets[i], ownerNetwork, defaultNamespace); err != nil {
			return fmt.Errorf("mint kubeconfig secret %q: %w", secrets[i].Name, err)
		}
	}
	return nil
}

func mintOneKubeconfigSecret(ctx context.Context, k KubeconfigSecretEntity, ownerNetwork, defaultNamespace string) error {
	key := k.Key
	if key == "" {
		key = "kubeconfig"
	}
	ns := k.Namespace
	if ns == "" {
		ns = defaultNamespace
	}
	if ns == "" {
		return fmt.Errorf("no namespace: set KubeconfigSecret.namespace or ensure the env declares one")
	}

	// 1. Get the target cluster's kubeconfig into a temp file we can
	//    rewrite without touching the user's ~/.kube/config.
	rawKubeconfig, err := k3dKubeconfigGet(ctx, k.TargetCluster)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "forge-kubeconfig-*.yaml")
	if err != nil {
		return fmt.Errorf("temp kubeconfig: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, werr := tmp.Write(rawKubeconfig); werr != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp kubeconfig: %w", werr)
	}
	if cerr := tmp.Close(); cerr != nil {
		return fmt.Errorf("close temp kubeconfig: %w", cerr)
	}

	// k3d names the kubeconfig context `k3d-<target>`; this is the name we
	// rewrite cluster/server on and rename to the stable context_name.
	k3dContext := "k3d-" + k.TargetCluster
	// The cluster ENTRY name inside a k3d kubeconfig is also `k3d-<target>`.
	clusterEntry := "k3d-" + k.TargetCluster

	if k.Reachability == "in-network" {
		// 2a. Resolve the target's API endpoint FRESH from docker — the
		//     serverlb container's IP on the owner network (with a
		//     server-0 fallback). Never persisted to a committed file.
		ip, ierr := resolveInNetworkAPIServerIP(ctx, k.TargetCluster, ownerNetwork)
		if ierr != nil {
			return ierr
		}
		server := fmt.Sprintf("https://%s:6443", ip)
		// Rewrite cluster.server, drop the CA (the serverlb cert doesn't
		// cover the container IP), and skip TLS verify.
		if err := kubectlConfigSetCluster(ctx, tmpPath, clusterEntry, server); err != nil {
			return err
		}
		if err := kubectlConfigUnsetCA(ctx, tmpPath, clusterEntry); err != nil {
			return err
		}
		if err := kubectlConfigSetInsecure(ctx, tmpPath, clusterEntry); err != nil {
			return err
		}
	}
	// 2b. "endpoint": leave the kubeconfig's own server/CA verbatim.

	// 3. Rename the context to the stable context_name the workload selects.
	if k.ContextName != k3dContext {
		if err := kubectlConfigRenameContext(ctx, tmpPath, k3dContext, k.ContextName); err != nil {
			return err
		}
	}

	// 4. Read the rewritten kubeconfig and apply it as a Secret in
	//    in_cluster. EnsureNamespace first (the namespace may not exist
	//    yet on a fresh cluster — same ordering fix the dotenv Secret
	//    apply uses).
	final, rerr := os.ReadFile(tmpPath)
	if rerr != nil {
		return fmt.Errorf("read minted kubeconfig: %w", rerr)
	}
	if err := cluster.EnsureNamespace(ctx, k.InCluster, ns); err != nil {
		return fmt.Errorf("ensure namespace %q in %q: %w", ns, k.InCluster, err)
	}
	secretYAML := kubeconfigSecretYAML(k.Name, ns, key, final)
	fmt.Printf("  applying kubeconfig Secret %s/%s into %s (target=%s, reachability=%s)\n",
		ns, k.Name, k.InCluster, k.TargetCluster, k.Reachability)
	if err := cluster.KubectlApply(ctx, k.InCluster, secretYAML); err != nil {
		return fmt.Errorf("apply kubeconfig Secret: %w", err)
	}
	return nil
}

// k3dKubeconfigGet returns the target cluster's kubeconfig YAML.
func k3dKubeconfigGet(ctx context.Context, target string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "k3d", "kubeconfig", "get", target).Output()
	if err != nil {
		return nil, fmt.Errorf("k3d kubeconfig get %s: %w", target, err)
	}
	return out, nil
}

// resolveInNetworkAPIServerIP returns the target cluster's API-server
// container IP on the shared owner network. Prefers the serverlb
// (load-balancer) container; falls back to server-0 when the cluster was
// created with --no-lb (no serverlb). The IP is read from docker every
// run and never written to a committed file — that's the whole point
// (the IP drifts each `k3d cluster create`).
func resolveInNetworkAPIServerIP(ctx context.Context, target, ownerNetwork string) (string, error) {
	if ownerNetwork == "" {
		return "", fmt.Errorf(
			"cannot resolve in-network IP for %q: no owner network "+
				"(the clusters must share a docker network — declare Cluster.network)", target)
	}
	format := fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, ownerNetwork)
	// serverlb first.
	if ip := dockerInspectIP(ctx, "k3d-"+target+"-serverlb", format); ip != "" {
		return ip, nil
	}
	// server-0 fallback (--no-lb clusters).
	if ip := dockerInspectIP(ctx, "k3d-"+target+"-server-0", format); ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf(
		"could not resolve API-server IP for cluster %q on network %q "+
			"(tried k3d-%s-serverlb and k3d-%s-server-0)", target, ownerNetwork, target, target)
}

// dockerInspectIP returns the inspected IP for a container, or "" when
// the container doesn't exist / isn't on the network (so the caller can
// fall through to the next candidate).
func dockerInspectIP(ctx context.Context, container, format string) string {
	out, err := exec.CommandContext(ctx, "docker", "inspect", container, "--format", format).Output()
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(out))
	// A container that exists but isn't on the queried network renders the
	// template to "<no value>" or empty — treat both as a miss.
	if ip == "" || ip == "<no value>" {
		return ""
	}
	return ip
}

// kubectlConfig* helpers operate on an explicit kubeconfig file (never
// the user's default config) via `kubectl config --kubeconfig=<path>`.

func kubectlConfigSetCluster(ctx context.Context, kubeconfigPath, clusterEntry, server string) error {
	return runKubectlConfig(ctx, kubeconfigPath,
		"set-cluster", clusterEntry, "--server="+server)
}

func kubectlConfigUnsetCA(ctx context.Context, kubeconfigPath, clusterEntry string) error {
	// Unset both the embedded CA data and any CA file path so the insecure
	// flag is the sole TLS policy.
	if err := runKubectlConfig(ctx, kubeconfigPath,
		"unset", "clusters."+clusterEntry+".certificate-authority-data"); err != nil {
		return err
	}
	return runKubectlConfig(ctx, kubeconfigPath,
		"unset", "clusters."+clusterEntry+".certificate-authority")
}

func kubectlConfigSetInsecure(ctx context.Context, kubeconfigPath, clusterEntry string) error {
	return runKubectlConfig(ctx, kubeconfigPath,
		"set-cluster", clusterEntry, "--insecure-skip-tls-verify=true")
}

func kubectlConfigRenameContext(ctx context.Context, kubeconfigPath, from, to string) error {
	return runKubectlConfig(ctx, kubeconfigPath, "rename-context", from, to)
}

// runKubectlConfig runs a `kubectl config` subcommand against an explicit
// kubeconfig file. Stderr is surfaced on failure (kubectl's config errors
// are actionable: "no cluster named X").
func runKubectlConfig(ctx context.Context, kubeconfigPath string, args ...string) error {
	all := append([]string{"config", "--kubeconfig", kubeconfigPath}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", all...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl config %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// kubeconfigSecretYAML builds the Opaque Secret manifest carrying the
// minted kubeconfig under `key`. Uses base64 `data` (not stringData) so
// the YAML stays safe regardless of the kubeconfig's contents.
func kubeconfigSecretYAML(name, namespace, key string, kubeconfig []byte) string {
	enc := base64.StdEncoding.EncodeToString(kubeconfig)
	var b strings.Builder
	b.WriteString("apiVersion: v1\n")
	b.WriteString("kind: Secret\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + name + "\n")
	b.WriteString("  namespace: " + namespace + "\n")
	b.WriteString("  labels:\n")
	b.WriteString("    app.kubernetes.io/managed-by: forge\n")
	b.WriteString("type: Opaque\n")
	b.WriteString("data:\n")
	b.WriteString("  " + key + ": " + enc + "\n")
	return b.String()
}

// ownerNetworkFromClusters derives the docker network the in-network IP
// resolution runs on. A secondary cluster names its owner's network via
// Cluster.network; the owner cluster gets its own auto-created network
// `k3d-<owner>`. The owner network is the value the secondaries point at —
// the first non-empty Cluster.network. When no cluster declares a
// network, fall back to the single declared cluster's own network
// (`k3d-<name>`), which is what a one-cluster env's serverlb sits on.
//
// There is NO "primary" notion: this just reads the network the env's
// clusters explicitly share (the secondaries' `network`), or — absent any
// cross-cluster wiring — the lone cluster's own k3d network.
func ownerNetworkFromClusters(clusters []ClusterEntity) string {
	for _, c := range clusters {
		if c.Network != "" {
			return c.Network
		}
	}
	if len(clusters) == 1 {
		return "k3d-" + clusters[0].Name
	}
	return ""
}
