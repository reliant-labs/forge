package cli

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestSafeModeK3sArgs(t *testing.T) {
	got, err := safeModeK3sArgs([]string{
		"server", "--disable=traefik", "--disable-network-policy=false", "--tls-san", "k3d-cp-serverlb",
	})
	if err != nil {
		t.Fatalf("safeModeK3sArgs: %v", err)
	}
	want := []string{"server", "--disable=traefik", "--tls-san", "k3d-cp-serverlb", "--disable-network-policy"}
	if !slices.Equal(got, want) {
		t.Fatalf("safe args = %v; want %v", got, want)
	}
	if _, err := safeModeK3sArgs([]string{"agent"}); err == nil {
		t.Fatal("agent args unexpectedly accepted for server repair")
	}
}

func TestFindK3dEntrypointPID(t *testing.T) {
	const processes = `PID COMMAND
    1 /sbin/docker-init -- /bin/k3d-entrypoint.sh server --tls-san 0.0.0.0
    7 {k3d-entrypoint.} /bin/sh /bin/k3d-entrypoint.sh server --tls-san 0.0.0.0
   39 /bin/k3s server
`
	pid, err := findK3dEntrypointPID(processes)
	if err != nil {
		t.Fatalf("findK3dEntrypointPID: %v", err)
	}
	if pid != 7 {
		t.Fatalf("entrypoint PID = %d; want 7", pid)
	}
}

func TestK3dNodeCurrentIP(t *testing.T) {
	var info dockerK3dNodeInspect
	info.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{
		"k3d-control-plane": {IPAddress: "172.27.0.5"},
	}
	got, err := k3dNodeCurrentIP(info, ClusterEntity{
		Name: "cp-daemon", Network: "k3d-control-plane", RegistryInherit: true,
	})
	if err != nil {
		t.Fatalf("k3dNodeCurrentIP: %v", err)
	}
	if got != "172.27.0.5" {
		t.Fatalf("current IP = %q; want 172.27.0.5", got)
	}
}

func TestK3sNodeIPDriftFailureSignature(t *testing.T) {
	logs := `time="2026-08-09T17:43:27Z" level=error msg="Shutdown request received: \"failed to start networking: unable to initialize network policy controller: error getting node subnet: failed to find interface with specified node ip\""`
	if !strings.Contains(logs, k3sNodeIPDriftFailure) {
		t.Fatal("known node-IP drift log was not recognized")
	}
}

func TestEnsureRunningK3dClusterHealthyRepairsKnownNodeIPDrift(t *testing.T) {
	origReady := runningClusterNodesReadyFn
	origProcess := containerHasK3sProcessFn
	origInspect := inspectDockerK3dNodeFn
	origLogs := dockerLogsSinceStartFn
	origRepair := repairK3sNodeIPDriftFn
	t.Cleanup(func() {
		runningClusterNodesReadyFn = origReady
		containerHasK3sProcessFn = origProcess
		inspectDockerK3dNodeFn = origInspect
		dockerLogsSinceStartFn = origLogs
		repairK3sNodeIPDriftFn = origRepair
	})

	runningClusterNodesReadyFn = func(context.Context, string) bool { return false }
	containerHasK3sProcessFn = func(context.Context, string) (bool, error) { return false, nil }
	inspectDockerK3dNodeFn = func(_ context.Context, node string) (dockerK3dNodeInspect, error) {
		if node != "k3d-cp-daemon-server-0" {
			t.Fatalf("inspect node = %q", node)
		}
		var info dockerK3dNodeInspect
		info.Args = []string{"server", "--tls-san", "k3d-cp-daemon-serverlb"}
		info.State.StartedAt = "2026-08-09T17:43:24Z"
		info.NetworkSettings.Networks = map[string]struct {
			IPAddress string `json:"IPAddress"`
		}{"k3d-control-plane": {IPAddress: "172.27.0.5"}}
		return info, nil
	}
	dockerLogsSinceStartFn = func(_ context.Context, node, startedAt string) (string, error) {
		if node != "k3d-cp-daemon-server-0" || startedAt != "2026-08-09T17:43:24Z" {
			t.Fatalf("logs args = node %q startedAt %q", node, startedAt)
		}
		return "Shutdown request received: " + k3sNodeIPDriftFailure, nil
	}
	repaired := false
	repairK3sNodeIPDriftFn = func(_ context.Context, c ClusterEntity, node, currentIP string, args []string) error {
		repaired = true
		if c.Name != "cp-daemon" || node != "k3d-cp-daemon-server-0" || currentIP != "172.27.0.5" {
			t.Fatalf("repair args = cluster %q node %q IP %q", c.Name, node, currentIP)
		}
		if !slices.Equal(args, []string{"server", "--tls-san", "k3d-cp-daemon-serverlb"}) {
			t.Fatalf("repair container args = %v", args)
		}
		return nil
	}

	err := ensureRunningK3dClusterHealthy(t.Context(), ClusterEntity{
		Name: "cp-daemon", Network: "k3d-control-plane", RegistryInherit: true, Servers: 1,
	})
	if err != nil {
		t.Fatalf("ensureRunningK3dClusterHealthy: %v", err)
	}
	if !repaired {
		t.Fatal("known node-IP drift did not trigger repair")
	}
}

func TestEnsureRunningK3dClusterHealthyReadyFastPath(t *testing.T) {
	origReady := runningClusterNodesReadyFn
	origProcess := containerHasK3sProcessFn
	t.Cleanup(func() {
		runningClusterNodesReadyFn = origReady
		containerHasK3sProcessFn = origProcess
	})

	runningClusterNodesReadyFn = func(_ context.Context, name string) bool {
		return name == "control-plane"
	}
	containerHasK3sProcessFn = func(context.Context, string) (bool, error) {
		t.Fatal("healthy fast path inspected the container process")
		return false, nil
	}
	if err := ensureRunningK3dClusterHealthy(t.Context(), ClusterEntity{Name: "control-plane"}); err != nil {
		t.Fatalf("healthy fast path: %v", err)
	}
}
