// Package cli — declarative cluster reconcile for `forge up`.
//
// This file generalizes the dev-only, single-cluster ensureDevCluster
// bootstrap into a declarative LIST: an env declares
// `Bundle.clusters = [forge.Cluster {...}, ...]` and forge ensures each
// exists at the head of `forge up` (create-if-absent, no-op if present).
//
// Multi-cluster ownership is a REFERENCE. There is no "primary" cluster:
// a secondary cluster names its `owner` Cluster, and the KCL render layer
// DERIVES the joined docker network (Cluster.Network = `k3d-<owner.name>`)
// and the registry-inherit flag (Cluster.RegistryInherit = true) from
// that one edge. The owner cluster projects neither — k3d creates its own
// network/registry. There is no most-X heuristic.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
// projectDir + env are threaded through for the per-cluster ingress
// install: a cluster that declares `ingress = True` gets the Gateway API
// stack installed into it after ensure, with entrypoints derived from the
// env's Gateway listeners. Both are empty-tolerant — a cluster with
// ingress off ignores them entirely.
func reconcileDeclaredClusters(ctx context.Context, clusters []ClusterEntity, projectDir, env string) error {
	if len(clusters) == 0 {
		return nil
	}
	fmt.Printf("\n[up] cluster phase — ensuring %d declared cluster(s)\n", len(clusters))
	for i := range clusters {
		if err := ensureDeclaredCluster(ctx, clusters[i], projectDir, env); err != nil {
			return fmt.Errorf("ensure cluster %q: %w", clusters[i].Name, err)
		}
	}
	return nil
}

// ensureDeclaredCluster creates one declared k3d cluster if it's absent,
// applying the declared flags; no-op when it already exists. After a
// fresh create of a cluster with RegistryInherit (an `owner` reference),
// it mirrors the owner cluster's registries.yaml onto the new node and
// restarts it to reload.
// clusterExistsFn / installClusterIngressFn are indirection seams so the
// reconcile decision logic is unit-testable without shelling out to k3d /
// kubectl. Production wires them to the real implementations; tests stub
// them to assert the ingress install is invoked exactly when (and only
// when) a cluster declares `ingress = True`.
var (
	clusterExistsFn             = clusterExists
	installClusterIngressFn     = installClusterIngress
	setupSecondaryClusterNodeFn = setupSecondaryClusterNode
)

// isNestedSecondary reports whether a cluster is a SECONDARY joined to an
// owner cluster's docker network — i.e. it declared an `owner`, which the
// render layer projects as RegistryInherit=true + a derived Network
// (`k3d-<owner.name>`). RegistryInherit keys directly off `owner != None`,
// so it's the load-bearing signal; Network carries the owner the node
// setup needs. Such a cluster needs the extra node setup
// setupSecondaryClusterNode performs.
func isNestedSecondary(c ClusterEntity) bool {
	return c.RegistryInherit && c.Network != ""
}

func ensureDeclaredCluster(ctx context.Context, c ClusterEntity, projectDir, env string) error {
	exists, err := clusterExistsFn(ctx, c.Name)
	if err != nil {
		return err
	}
	if exists {
		fmt.Printf("  cluster %q already exists — no-op\n", c.Name)
		// Re-run the ingress install on a warm cluster too: it's
		// idempotent (kubectl apply + CRD cache), and a cluster that was
		// created before `ingress` was declared — or had its Traefik
		// deleted — heals on the next `forge up`.
		if c.Ingress {
			if err := installClusterIngressFn(ctx, c, projectDir, env); err != nil {
				return err
			}
		}
		// Re-run the secondary-cluster node setup on warm runs too. Every
		// step is idempotent (a guard check precedes each mutation), so a
		// cluster whose host-gateway DNS alias or MSS clamp was lost (e.g.
		// a manual node restart cleared the iptables rule) heals on the
		// next `forge up`.
		if isNestedSecondary(c) {
			if err := setupSecondaryClusterNodeFn(ctx, c); err != nil {
				return err
			}
		}
		return nil
	}

	args := []string{"cluster", "create", c.Name}
	if c.Config != "" {
		// A config file is authoritative: k3d reads servers/agents/ports/
		// network/registries from it. We still pass the cluster NAME
		// positionally (k3d merges it with metadata.name) so clusterExists
		// can find it by the declared name afterward.
		//
		// When this cluster hosts the ingress Gateway (c.Ingress), merge the
		// generated deploy/k3d-ports.yaml listener host-port fragment into the
		// config the SAME way the dev `forge cluster up` path does. The
		// Gateway's listeners (e.g. grpc :29190 for daemon dial-out) need host
		// ports mapped through the k3d loadbalancer at CREATE time, or
		// in-cluster→host→cluster traffic (host.k3d.internal:<port>) finds
		// nothing listening. k3d.yaml deliberately carries no `ports:` block —
		// the ports live in the generated fragment, merged here.
		// Ensure any standalone registry the config references via
		// `registries.use` exists BEFORE create — a `use` reference fails the
		// create if the registry container isn't already up. A standalone
		// registry is owned by no cluster, so it (and its pushed images)
		// survives a later `cluster delete`; the next cold create re-references
		// the SAME registry and image pushes are cache hits. Idempotent: a
		// present registry is a `k3d registry list` no-op. Reads the un-merged
		// user config (the ports merge below doesn't touch `registries`).
		if err := ensureConfigRegistries(ctx, c.Config); err != nil {
			return fmt.Errorf("ensure standalone registry for cluster %q: %w", c.Name, err)
		}

		cfgPath, cleanup, err := mergeK3dConfig(c.Config, c.Ingress)
		if err != nil {
			return fmt.Errorf("merge k3d ports for cluster %q: %w", c.Name, err)
		}
		defer cleanup()
		if cfgPath.temporary {
			fmt.Printf("  (merging deploy/k3d-ports.yaml — Gateway listener host ports)\n")
		}
		args = append(args, "--config", cfgPath.path)
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

	if c.RegistryInherit {
		if c.Network == "" {
			return fmt.Errorf(
				"cluster %q: registry_inherit is set but the derived network is empty — an `owner` Cluster "+
					"reference is required so forge can resolve the owner whose registry to mirror", c.Name)
		}
		if err := inheritRegistryMirror(ctx, c); err != nil {
			return fmt.Errorf("inherit registry mirror: %w", err)
		}
	}

	// A secondary cluster nested on an owner's docker network needs node
	// setup beyond the registry mirror: the owner's host-gateway DNS alias
	// (so its pods reach host services) and a TCP MSS clamp (so the nested
	// path-MTU doesn't shred large outbound TLS handshakes). Run it AFTER
	// inheritRegistryMirror, which restarts the node — the MSS clamp is
	// applied to the node's live iptables and must outlive that restart.
	if isNestedSecondary(c) {
		if err := setupSecondaryClusterNodeFn(ctx, c); err != nil {
			return err
		}
	}

	// Install the Gateway API stack into the freshly-created cluster when
	// it declares `ingress = True`. A fresh `k3d cluster create` ships no
	// Gateway API CRDs / no Gateway controller, so a cluster that hosts
	// the env's Gateway/HTTPRoute/GRPCRoute resources needs the stack
	// installed before the deploy phase applies them.
	if c.Ingress {
		if err := installClusterIngressFn(ctx, c, projectDir, env); err != nil {
			return err
		}
	}
	return nil
}

// installClusterIngress installs the Gateway API stack (pinned
// Gateway-API CRDs + the vendored Traefik controller + the `traefik`
// GatewayClass) into the named declared cluster, targeting its
// `k3d-<name>` kubectl context explicitly so it lands on THAT cluster
// regardless of the active context. Reuses the same installIngressBundle
// the dev `forge cluster up` path uses; idempotent on warm clusters.
func installClusterIngress(ctx context.Context, c ClusterEntity, projectDir, env string) error {
	kctx := "k3d-" + c.Name
	fmt.Printf("  installing Gateway API + Traefik ingress into cluster %q (context %s)...\n", c.Name, kctx)
	if err := installIngressBundle(ctx, kctx, projectDir, env); err != nil {
		return fmt.Errorf("install ingress on cluster %q: %w", c.Name, err)
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

// setupSecondaryClusterNode performs the node setup a SECONDARY cluster
// (nested on an owner's docker network) needs but that `k3d cluster
// create` does not do — beyond the registry mirror inheritRegistryMirror
// already handles. Two steps, both idempotent (a guard precedes each
// mutation so warm re-runs no-op):
//
//  1. HOST-GATEWAY DNS. The owner cluster reaches host services (a
//     host-mode Postgres/NATS, etc.) via `host.k3d.internal`, which k3d
//     seeds in the OWNER node's /etc/hosts and CoreDNS NodeHosts. The
//     secondary, joined to the owner's network, gets no such alias — and
//     the registry-mirror node restart regenerates CoreDNS from
//     /etc/hosts, dropping anything k3d injected. Without it the
//     secondary's pods fail "lookup host.k3d.internal: no such host". We
//     copy the alias FROM the owner cluster's CoreDNS NodeHosts (rather
//     than hardcoding a host-gateway IP) so it tracks whatever gateway IP
//     this docker host actually uses, then write it into the secondary's
//     node /etc/hosts + CoreDNS NodeHosts and bounce CoreDNS.
//
//  2. TCP MSS CLAMP. A secondary nested on the owner's docker network has
//     a lower effective egress path-MTU (pod → secondary node → shared
//     bridge → host NAT) than the pod's interface MTU. Large TLS
//     handshake packets are dropped with DF set, so outbound TLS (e.g. a
//     git clone over HTTPS) dies "gnutls_handshake() failed: The TLS
//     connection was non-properly terminated." Clamp TCP MSS on the
//     node's FORWARD chain so sessions negotiate a fitting segment.
//     Applied AFTER the registry-mirror node restart — iptables state
//     does not survive it. Idempotent via an iptables -C check.
func setupSecondaryClusterNode(ctx context.Context, c ClusterEntity) error {
	owner := strings.TrimPrefix(c.Network, "k3d-")
	if owner == "" {
		return fmt.Errorf("cluster %q: cannot derive owner cluster from network %q", c.Name, c.Network)
	}
	node := "k3d-" + c.Name + "-server-0"

	if err := ensureHostGatewayDNS(ctx, c, owner, node); err != nil {
		return fmt.Errorf("ensure host-gateway DNS on %s: %w", node, err)
	}
	if err := ensureMSSClamp(ctx, node); err != nil {
		return fmt.Errorf("clamp TCP MSS on %s: %w", node, err)
	}
	return nil
}

// ensureHostGatewayDNS copies the `host.k3d.internal` alias from the OWNER
// cluster's CoreDNS NodeHosts onto the secondary cluster's node /etc/hosts
// and CoreDNS NodeHosts, then bounces CoreDNS. Idempotent: a no-op when
// the secondary already resolves the alias. Reading the IP from the owner
// (instead of hardcoding the Docker Desktop gateway) keeps it portable
// across docker hosts.
func ensureHostGatewayDNS(ctx context.Context, c ClusterEntity, owner, node string) error {
	const alias = "host.k3d.internal"
	ownerKctx := "k3d-" + owner
	secKctx := "k3d-" + c.Name

	// Resolve the host-gateway line from the owner's CoreDNS NodeHosts —
	// e.g. "192.168.65.254 host.k3d.internal". This is the source of truth
	// for whatever gateway IP this docker host assigned.
	ownerHosts, err := readCoreDNSNodeHosts(ctx, ownerKctx)
	if err != nil {
		return fmt.Errorf("read owner %s CoreDNS NodeHosts: %w", ownerKctx, err)
	}
	gwLine := nodeHostsLineFor(ownerHosts, alias)
	if gwLine == "" {
		// The owner doesn't advertise the alias (e.g. a docker host where
		// k3d didn't seed it). Nothing to copy; skip rather than guess an IP.
		fmt.Printf("  owner cluster %q has no %s alias — skipping host-gateway DNS for %q\n", owner, alias, c.Name)
		return nil
	}

	// Node /etc/hosts: append the line if absent.
	addHosts := fmt.Sprintf("grep -q %q /etc/hosts || echo %q >> /etc/hosts", alias, gwLine)
	if err := execNodeShell(ctx, node, addHosts); err != nil {
		return fmt.Errorf("append %s to %s /etc/hosts: %w", alias, node, err)
	}

	// CoreDNS NodeHosts: append the line + bounce CoreDNS, only if absent.
	secHosts, err := readCoreDNSNodeHosts(ctx, secKctx)
	if err != nil {
		return fmt.Errorf("read %s CoreDNS NodeHosts: %w", secKctx, err)
	}
	if nodeHostsLineFor(secHosts, alias) != "" {
		fmt.Printf("  %s already resolves %s in %q — no-op\n", alias, alias, c.Name)
		return nil
	}
	newHosts := strings.TrimRight(secHosts, "\n") + "\n" + gwLine
	fmt.Printf("  adding %q to cluster %q CoreDNS NodeHosts (copied from owner %q) and bouncing CoreDNS...\n", gwLine, c.Name, owner)
	if err := patchCoreDNSNodeHosts(ctx, secKctx, newHosts); err != nil {
		return err
	}
	return rolloutRestartCoreDNS(ctx, secKctx)
}

// ensureMSSClamp adds a TCP MSS-clamp rule to the node's mangle/FORWARD
// chain, idempotently via an iptables -C check (append only when the
// rule isn't already present).
func ensureMSSClamp(ctx context.Context, node string) error {
	const rule = "-p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1360"
	// `iptables -C` (check) exits non-zero when the rule is absent; only
	// then do we append. Run as one shell so the check+append is atomic
	// from the host's perspective.
	clamp := fmt.Sprintf(
		"iptables -t mangle -C FORWARD %s 2>/dev/null || iptables -t mangle -A FORWARD %s",
		rule, rule)
	fmt.Printf("  clamping TCP MSS on %s (nested-network MTU fix for outbound TLS)...\n", node)
	return execNodeShell(ctx, node, clamp)
}

// readCoreDNSNodeHosts returns the kube-system/coredns ConfigMap's
// NodeHosts data for the given kubectl context (empty string when unset).
func readCoreDNSNodeHosts(ctx context.Context, kctx string) (string, error) {
	out, err := exec.CommandContext(ctx, "kubectl", "--context", kctx,
		"get", "configmap", "coredns", "-n", "kube-system",
		"-o", "jsonpath={.data.NodeHosts}").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// nodeHostsLineFor returns the (trimmed) NodeHosts line whose host column
// contains the alias, or "" when absent. NodeHosts is `<ip> <name>...`
// lines; we match on a whitespace-delimited field so a substring like
// `host.k3d.internal` doesn't false-match a longer hostname.
func nodeHostsLineFor(nodeHosts, alias string) string {
	for _, line := range strings.Split(nodeHosts, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// fields[0] is the IP; the rest are hostnames for that IP.
		for _, host := range fields[1:] {
			if host == alias {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// patchCoreDNSNodeHosts writes newHosts as the coredns ConfigMap's
// NodeHosts via a strategic-merge patch (avoids a get-mutate-apply race on
// the rest of the ConfigMap).
func patchCoreDNSNodeHosts(ctx context.Context, kctx, newHosts string) error {
	patch := map[string]any{"data": map[string]string{"NodeHosts": newHosts}}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "kubectl", "--context", kctx,
		"patch", "configmap", "coredns", "-n", "kube-system",
		"--type", "merge", "-p", string(body))
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// rolloutRestartCoreDNS bounces the coredns deployment so it reloads the
// patched NodeHosts.
func rolloutRestartCoreDNS(ctx context.Context, kctx string) error {
	cmd := exec.CommandContext(ctx, "kubectl", "--context", kctx,
		"rollout", "restart", "deployment/coredns", "-n", "kube-system")
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// execNodeShell runs a /bin/sh -c command inside a k3d node container.
func execNodeShell(ctx context.Context, node, script string) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", node, "sh", "-c", script)
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
