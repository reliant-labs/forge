// Package cli — declarative cluster reconcile for `forge up`.
//
// This file generalizes the dev-only, single-cluster ensureDevCluster
// bootstrap into a declarative LIST: an env declares
// `Bundle.clusters = [forge.Cluster {...}, ...]` and forge ensures each
// exists at the head of `forge up` (create-if-absent, no-op if present).
//
// Multi-cluster ownership is IMPLICIT. There is no "primary" cluster: a
// secondary cluster names its owner via Cluster.Network (the k3d docker
// network it joins, `k3d-<owner>`) and reuses the owner's registry via
// Cluster.RegistryMirror == "inherit". The owner declares neither — k3d
// creates its own network/registry. There is no most-X heuristic.
package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// reconcileDeclaredClusters ensures every cluster the env declares
// exists, in declaration order. Idempotent: a cluster that already
// exists is a no-op (warm-run fast path). The owner cluster must be
// declared BEFORE any secondary that inherits its network/registry —
// declaration order is the contract (a secondary references the owner's
// network by name, which only exists once the owner is created).
//
// A nil/empty list is a no-op: an env that declares no clusters keeps
// today's behavior (`forge up --env=e2e` ensures nothing; the legacy
// single dev cluster is bootstrapped by ensureDevCluster on the deploy
// path).
func reconcileDeclaredClusters(ctx context.Context, clusters []ClusterEntity) error {
	if len(clusters) == 0 {
		return nil
	}
	fmt.Printf("\n[up] cluster phase — ensuring %d declared cluster(s)\n", len(clusters))
	for i := range clusters {
		if err := ensureDeclaredCluster(ctx, clusters[i]); err != nil {
			return fmt.Errorf("ensure cluster %q: %w", clusters[i].Name, err)
		}
	}
	return nil
}

// ensureDeclaredCluster creates one declared k3d cluster if it's absent,
// applying the declared flags; no-op when it already exists. After a
// fresh create with RegistryMirror=="inherit", it mirrors the owner
// cluster's registries.yaml onto the new node and restarts it to reload.
func ensureDeclaredCluster(ctx context.Context, c ClusterEntity) error {
	exists, err := clusterExists(ctx, c.Name)
	if err != nil {
		return err
	}
	if exists {
		fmt.Printf("  cluster %q already exists — no-op\n", c.Name)
		return nil
	}

	args := []string{"cluster", "create", c.Name}
	if c.Config != "" {
		// A config file is authoritative: k3d reads servers/agents/ports/
		// network/registries from it. We still pass the cluster NAME
		// positionally (k3d merges it with metadata.name) so clusterExists
		// can find it by the declared name afterward.
		args = append(args, "--config", c.Config)
	} else {
		args = append(args, "--servers", strconv.Itoa(effectiveServers(c)))
		if c.Agents > 0 {
			args = append(args, "--agents", strconv.Itoa(c.Agents))
		}
		if c.Network != "" {
			args = append(args, "--network", c.Network)
		}
		if c.APIPort > 0 {
			// Bind on 0.0.0.0 so a sibling cluster on the same docker
			// network can reach this API server by host IP if needed.
			args = append(args, "--api-port", fmt.Sprintf("0.0.0.0:%d", c.APIPort))
		}
	}

	fmt.Printf("  creating k3d cluster %q (%s)...\n", c.Name, strings.Join(args[2:], " "))
	create := exec.CommandContext(ctx, "k3d", args...)
	create.Stdout = os.Stdout
	create.Stderr = os.Stderr
	if err := create.Run(); err != nil {
		return fmt.Errorf("k3d cluster create: %w", err)
	}

	if c.RegistryMirror == "inherit" {
		if c.Network == "" {
			return fmt.Errorf(
				"cluster %q: registry_mirror=\"inherit\" requires `network` set to the owner cluster's network "+
					"(k3d names it k3d-<owner>) so forge can resolve the owner whose registry to mirror", c.Name)
		}
		if err := inheritRegistryMirror(ctx, c); err != nil {
			return fmt.Errorf("inherit registry mirror: %w", err)
		}
	}
	return nil
}

// effectiveServers defaults a zero Servers to 1 (the schema default;
// belt-and-suspenders for a hand-constructed entity that skipped it).
func effectiveServers(c ClusterEntity) int {
	if c.Servers <= 0 {
		return 1
	}
	return c.Servers
}

// inheritRegistryMirror ports scripts/bootstrap-dev-daemon.sh's
// registries.yaml load-order fix to Go: copy the OWNER cluster's
// containerd registries.yaml onto THIS cluster's server node and restart
// it so containerd reloads with the mirror. The owner is the cluster
// whose auto-created network this one joined — k3d names that network
// `k3d-<owner>`, so the owner name is Network with the `k3d-` prefix
// stripped. forge owns the owner cluster's registry container, so the
// mapping is deterministic.
//
// Without this, in-cluster pulls of `localhost:<port>/<image>` from the
// secondary cluster's node fail (the registry endpoint is only resolvable
// via the shared mirror config). Copying the EXACT same registries.yaml
// the owner uses keeps the two clusters' pull behavior identical.
func inheritRegistryMirror(ctx context.Context, c ClusterEntity) error {
	owner := strings.TrimPrefix(c.Network, "k3d-")
	if owner == "" {
		return fmt.Errorf("cluster %q: cannot derive owner cluster from network %q", c.Name, c.Network)
	}
	ownerNode := "k3d-" + owner + "-server-0"
	newNode := "k3d-" + c.Name + "-server-0"

	// Read the owner node's registries.yaml. The owner is forge-created
	// (it carries the canonical mirror config), so this file exists.
	regsYAML, err := readNodeFile(ctx, ownerNode, "/etc/rancher/k3s/registries.yaml")
	if err != nil {
		// Fall back to forge's canonical fallback mirror config: an owner
		// created from a deploy/k3d.yaml carries the mirrors inline in
		// containerd's config rather than a standalone registries.yaml, so
		// a read miss isn't fatal — write the known-good fallback the
		// owner's inline config mirrors.
		regsYAML = []byte(fallbackRegistriesYAML)
	}
	if len(bytes.TrimSpace(regsYAML)) == 0 {
		regsYAML = []byte(fallbackRegistriesYAML)
	}

	fmt.Printf("  mirroring %s registries.yaml onto %s and reloading...\n", ownerNode, newNode)
	if err := writeNodeFile(ctx, newNode, "/etc/rancher/k3s/registries.yaml", regsYAML); err != nil {
		return fmt.Errorf("write registries.yaml onto %s: %w", newNode, err)
	}
	if err := dockerRestart(ctx, newNode); err != nil {
		return fmt.Errorf("restart %s: %w", newNode, err)
	}
	if err := waitNodeReady(ctx, c.Name); err != nil {
		return fmt.Errorf("wait %s ready after reload: %w", c.Name, err)
	}
	return nil
}

// readNodeFile cats a file from inside a k3d node container.
func readNodeFile(ctx context.Context, node, path string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "docker", "exec", node, "cat", path).Output()
	if err != nil {
		return nil, fmt.Errorf("docker exec %s cat %s: %w", node, path, err)
	}
	return out, nil
}

// writeNodeFile writes content to a path inside a k3d node container by
// piping it to `sh -c 'cat > path'` (the node image has no `tee`/`cp`
// from host). Matches the bootstrap script's heredoc approach.
func writeNodeFile(ctx context.Context, node, path string, content []byte) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", node, "sh", "-c", "cat > "+path)
	cmd.Stdin = bytes.NewReader(content)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// dockerRestart restarts a container (the k3d node) so containerd
// reloads its registries.yaml.
func dockerRestart(ctx context.Context, node string) error {
	cmd := exec.CommandContext(ctx, "docker", "restart", node)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// waitNodeReady blocks (bounded) until the cluster's nodes report Ready
// after a node restart — containerd needs a beat to come back. Uses the
// k3d context convention (k3d-<name>) so the wait targets the right
// cluster regardless of the active kubectl context. Best-effort with a
// hard cap; a slow-to-Ready node surfaces downstream as a deploy
// rollout failure with its own diagnostics rather than hanging here.
func waitNodeReady(ctx context.Context, clusterName string) error {
	deadline := time.Now().Add(90 * time.Second)
	kctx := "k3d-" + clusterName
	for {
		cmd := exec.CommandContext(ctx, "kubectl", "--context", kctx,
			"wait", "--for=condition=Ready", "nodes", "--all", "--timeout=15s")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nodes not Ready within 90s (context %s)", kctx)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
