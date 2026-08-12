package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const k3sNodeIPDriftFailure = "failed to find interface with specified node ip"

var (
	k3sNodeIPRepairTimeout      = 90 * time.Second
	k3sNodeIPRepairPollInterval = 2 * time.Second
	runningClusterNodesReadyFn  = clusterNodesReadyOnce
	containerHasK3sProcessFn    = containerHasK3sProcess
	inspectDockerK3dNodeFn      = inspectDockerK3dNode
	dockerLogsSinceStartFn      = dockerLogsSinceStart
	repairK3sNodeIPDriftFn      = repairK3sNodeIPDrift
)

// dockerK3dNodeInspect is the subset of `docker container inspect` needed to
// recover a k3d server after Docker assigns it a different dynamic IP. Docker
// reports the container as running even when the k3d entrypoint's k3s child
// has exited, so container state alone is not a Kubernetes health signal.
type dockerK3dNodeInspect struct {
	Args  []string `json:"Args"`
	State struct {
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// ensureRunningK3dClusterHealthy closes the gap between Docker liveness and
// Kubernetes liveness. In particular, k3d's entrypoint can remain alive in an
// uncordon retry loop after k3s fatally exits because Docker reassigned the
// node IP. For that exact, known failure we repair the persisted Kubernetes
// Node IP in place and then restart k3s normally with network policy enabled.
func ensureRunningK3dClusterHealthy(ctx context.Context, c ClusterEntity) error {
	if runningClusterNodesReadyFn(ctx, c.Name) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	node := "k3d-" + c.Name + "-server-0"
	k3sRunning, err := containerHasK3sProcessFn(ctx, node)
	if err != nil {
		return err
	}
	if k3sRunning {
		fmt.Printf("  cluster %q API is still starting — waiting for nodes to become Ready...\n", c.Name)
		return waitNodeReady(ctx, c.Name)
	}

	info, err := inspectDockerK3dNodeFn(ctx, node)
	if err != nil {
		return err
	}
	logs, err := dockerLogsSinceStartFn(ctx, node, info.State.StartedAt)
	if err != nil {
		return err
	}
	if !strings.Contains(logs, k3sNodeIPDriftFailure) {
		return fmt.Errorf(
			"container %s is running but its k3s process exited; the known Docker node-IP drift failure was not found in current-start logs (inspect with `docker logs %s`)",
			node, node)
	}
	if effectiveServers(c) != 1 || c.Agents != 0 {
		return fmt.Errorf(
			"detected Docker node-IP drift in %s, but automatic in-place repair is limited to single-server clusters without agents (servers=%d agents=%d)",
			node, effectiveServers(c), c.Agents)
	}

	currentIP, err := k3dNodeCurrentIP(info, c)
	if err != nil {
		return fmt.Errorf("resolve current IP for %s: %w", node, err)
	}
	fmt.Printf("  detected k3s node-IP drift in %q after Docker reassigned %s to %s — repairing in place...\n",
		c.Name, node, currentIP)
	return repairK3sNodeIPDriftFn(ctx, c, node, currentIP, info.Args)
}

// clusterNodesReadyOnce is a quick healthy-path probe. A healthy warm cluster
// returns immediately; an API that is merely starting gets a bounded grace
// period before the deeper process/log diagnosis runs.
func clusterNodesReadyOnce(ctx context.Context, clusterName string) bool {
	kctx := "k3d-" + clusterName
	cmd := exec.CommandContext(ctx, "kubectl", "--request-timeout=3s", "--context", kctx,
		"wait", "--for=condition=Ready", "nodes", "--all", "--timeout=5s")
	return cmd.Run() == nil
}

func containerHasK3sProcess(ctx context.Context, node string) (bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "exec", node, "pidof", "k3s").CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(out)) != "", nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("inspect k3s process in %s: %w: %s", node, err, strings.TrimSpace(string(out)))
}

func inspectDockerK3dNode(ctx context.Context, node string) (dockerK3dNodeInspect, error) {
	out, err := exec.CommandContext(ctx, "docker", "container", "inspect", node).CombinedOutput()
	if err != nil {
		return dockerK3dNodeInspect{}, fmt.Errorf("inspect %s: %w: %s", node, err, strings.TrimSpace(string(out)))
	}
	var inspected []dockerK3dNodeInspect
	if err := json.Unmarshal(out, &inspected); err != nil {
		return dockerK3dNodeInspect{}, fmt.Errorf("decode docker inspect for %s: %w", node, err)
	}
	if len(inspected) != 1 {
		return dockerK3dNodeInspect{}, fmt.Errorf("docker inspect for %s returned %d records; want 1", node, len(inspected))
	}
	return inspected[0], nil
}

func dockerLogsSinceStart(ctx context.Context, node, startedAt string) (string, error) {
	args := []string{"logs"}
	if startedAt != "" {
		args = append(args, "--since", startedAt)
	} else {
		args = append(args, "--tail", "500")
	}
	args = append(args, node)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read current-start logs for %s: %w: %s", node, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func k3dNodeCurrentIP(info dockerK3dNodeInspect, c ClusterEntity) (string, error) {
	network := c.Network
	if network == "" {
		network = "k3d-" + c.Name
	}
	if endpoint, ok := info.NetworkSettings.Networks[network]; ok && endpoint.IPAddress != "" {
		return endpoint.IPAddress, nil
	}
	if len(info.NetworkSettings.Networks) == 1 {
		for _, endpoint := range info.NetworkSettings.Networks {
			if endpoint.IPAddress != "" {
				return endpoint.IPAddress, nil
			}
		}
	}
	return "", fmt.Errorf("network %q has no container IP", network)
}

func safeModeK3sArgs(containerArgs []string) ([]string, error) {
	if len(containerArgs) == 0 || containerArgs[0] != "server" {
		return nil, fmt.Errorf("unexpected k3d server command args %q", containerArgs)
	}
	args := make([]string, 0, len(containerArgs)+1)
	for _, arg := range containerArgs {
		if arg == "--disable-network-policy" || strings.HasPrefix(arg, "--disable-network-policy=") {
			continue
		}
		args = append(args, arg)
	}
	return append(args, "--disable-network-policy"), nil
}

func findK3dEntrypointPID(processes string) (int, error) {
	for _, line := range strings.Split(processes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.Contains(strings.Join(fields[1:], " "), "/bin/k3d-entrypoint.sh") {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == 1 {
			continue
		}
		return pid, nil
	}
	return 0, fmt.Errorf("could not find non-PID-1 k3d entrypoint shell")
}

func k3dEntrypointPID(ctx context.Context, node string) (int, error) {
	out, err := exec.CommandContext(ctx, "docker", "exec", node, "ps", "-eo", "pid,args").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("list processes in %s: %w: %s", node, err, strings.TrimSpace(string(out)))
	}
	pid, err := findK3dEntrypointPID(string(out))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", node, err)
	}
	return pid, nil
}

// repairK3sNodeIPDrift performs the upstream-recommended safe-mode recovery:
// pause k3d's otherwise-infinite uncordon loop, start the same k3s server once
// with network policy disabled, wait for kubelet to publish the container's
// current IP, stop safe mode, and restart the original container command.
// Network policy is therefore disabled only during repair and is enabled again
// before this function returns.
func repairK3sNodeIPDrift(ctx context.Context, c ClusterEntity, node, currentIP string, containerArgs []string) error {
	safeArgs, err := safeModeK3sArgs(containerArgs)
	if err != nil {
		return err
	}
	entrypointPID, err := k3dEntrypointPID(ctx, node)
	if err != nil {
		return err
	}

	paused := false
	safeK3sStarted := false
	defer func() {
		if safeK3sStarted {
			_ = exec.Command("docker", "exec", node, "killall", "-TERM", "k3s").Run()
		}
		if paused {
			_ = exec.Command("docker", "exec", node, "kill", "-CONT", strconv.Itoa(entrypointPID)).Run()
		}
	}()

	if out, err := exec.CommandContext(ctx, "docker", "exec", node,
		"kill", "-STOP", strconv.Itoa(entrypointPID)).CombinedOutput(); err != nil {
		return fmt.Errorf("pause k3d entrypoint in %s: %w: %s", node, err, strings.TrimSpace(string(out)))
	}
	paused = true

	args := []string{"exec", "-d", node, "/bin/k3s"}
	args = append(args, safeArgs...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start temporary network-policy-disabled k3s in %s: %w", node, err)
	}
	safeK3sStarted = true

	if err := waitForK3sNodeInternalIP(ctx, c.Name, node, currentIP); err != nil {
		return err
	}

	if out, err := exec.CommandContext(ctx, "docker", "exec", node,
		"killall", "-TERM", "k3s").CombinedOutput(); err != nil {
		return fmt.Errorf("stop temporary k3s in %s: %w: %s", node, err, strings.TrimSpace(string(out)))
	}
	safeK3sStarted = false
	if out, err := exec.CommandContext(ctx, "docker", "exec", node,
		"kill", "-CONT", strconv.Itoa(entrypointPID)).CombinedOutput(); err != nil {
		return fmt.Errorf("resume k3d entrypoint in %s: %w: %s", node, err, strings.TrimSpace(string(out)))
	}
	paused = false

	fmt.Printf("  node %s now advertises %s — restarting with network policy enabled...\n", node, currentIP)
	if err := dockerRestart(ctx, node); err != nil {
		return fmt.Errorf("restart repaired node %s: %w", node, err)
	}
	if err := refreshK3dLoadBalancer(ctx, c.Name); err != nil {
		return fmt.Errorf("refresh %s load balancer after node-IP repair: %w", c.Name, err)
	}
	if err := waitNodeReady(ctx, c.Name); err != nil {
		return fmt.Errorf("wait for repaired cluster %q: %w", c.Name, err)
	}
	return nil
}

func waitForK3sNodeInternalIP(ctx context.Context, clusterName, node, wantIP string) error {
	deadline := time.NewTimer(k3sNodeIPRepairTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(k3sNodeIPRepairPollInterval)
	defer ticker.Stop()
	kctx := "k3d-" + clusterName

	for {
		out, err := exec.CommandContext(ctx, "kubectl", "--request-timeout=2s", "--context", kctx,
			"get", "node", node,
			"-o", "jsonpath={.status.addresses[?(@.type==\"InternalIP\")].address}").Output()
		if err == nil && strings.TrimSpace(string(out)) == wantIP {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("temporary k3s did not update node %s to current IP %s within %s", node, wantIP, k3sNodeIPRepairTimeout)
		case <-ticker.C:
		}
	}
}
