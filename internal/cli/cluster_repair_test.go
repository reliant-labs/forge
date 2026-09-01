package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
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
	origGrace := k3sProcessGrace
	t.Cleanup(func() {
		runningClusterNodesReadyFn = origReady
		containerHasK3sProcessFn = origProcess
		inspectDockerK3dNodeFn = origInspect
		dockerLogsSinceStartFn = origLogs
		repairK3sNodeIPDriftFn = origRepair
		k3sProcessGrace = origGrace
	})
	// k3s is stubbed permanently absent; don't pay the real startup grace.
	k3sProcessGrace = 0

	runningClusterNodesReadyFn = func(context.Context, string) (bool, error) {
		return false, errors.New("connection refused")
	}
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

	runningClusterNodesReadyFn = func(_ context.Context, name string) (bool, error) {
		return name == "control-plane", nil
	}
	containerHasK3sProcessFn = func(context.Context, string) (bool, error) {
		t.Fatal("healthy fast path inspected the container process")
		return false, nil
	}
	if err := ensureRunningK3dClusterHealthy(t.Context(), ClusterEntity{Name: "control-plane"}); err != nil {
		t.Fatalf("healthy fast path: %v", err)
	}
}

// TestEnsureRunningK3dClusterHealthySaysWhyTheProbeFailed pins the reporting
// contract that motivated the probe's signature: when the API does not confirm
// Ready, forge must say what it actually observed rather than assert an
// unestablished diagnosis. A run that only ever printed "API is still
// starting" for a cluster whose API was answering fine left the operator with
// no way to tell a slow start from a blip from a wedged host.
func TestEnsureRunningK3dClusterHealthySaysWhyTheProbeFailed(t *testing.T) {
	origReady := runningClusterNodesReadyFn
	origProcess := containerHasK3sProcessFn
	origAttempt := nodeReadyAttemptFn
	origBudget := nodeReadyWaitBudget
	t.Cleanup(func() {
		runningClusterNodesReadyFn = origReady
		containerHasK3sProcessFn = origProcess
		nodeReadyAttemptFn = origAttempt
		nodeReadyWaitBudget = origBudget
	})

	runningClusterNodesReadyFn = func(context.Context, string) (bool, error) {
		return false, errors.New("Unable to connect to the server: EOF")
	}
	containerHasK3sProcessFn = func(context.Context, string) (bool, error) { return true, nil }
	nodeReadyAttemptFn = func(context.Context, string, time.Duration) (string, error) { return "", nil }
	nodeReadyWaitBudget = time.Second

	out := captureStdout(t, func() {
		if err := ensureRunningK3dClusterHealthy(t.Context(), ClusterEntity{Name: "control-plane"}); err != nil {
			t.Fatalf("ensureRunningK3dClusterHealthy: %v", err)
		}
	})
	if !strings.Contains(out, "Unable to connect to the server: EOF") {
		t.Fatalf("probe reason was not reported; got:\n%s", out)
	}
}

// TestEnsureRunningK3dClusterHealthyCarriesProbeReasonIntoTheError covers the
// same contract on the error path: k3s exited for a reason forge cannot
// auto-repair, and the message has to carry BOTH halves of the evidence — the
// dead process and what Kubernetes said.
func TestEnsureRunningK3dClusterHealthyCarriesProbeReasonIntoTheError(t *testing.T) {
	origReady := runningClusterNodesReadyFn
	origProcess := containerHasK3sProcessFn
	origInspect := inspectDockerK3dNodeFn
	origLogs := dockerLogsSinceStartFn
	origGrace := k3sProcessGrace
	t.Cleanup(func() {
		runningClusterNodesReadyFn = origReady
		containerHasK3sProcessFn = origProcess
		inspectDockerK3dNodeFn = origInspect
		dockerLogsSinceStartFn = origLogs
		k3sProcessGrace = origGrace
	})
	// k3s is stubbed permanently absent; don't pay the real startup grace.
	k3sProcessGrace = 0

	runningClusterNodesReadyFn = func(context.Context, string) (bool, error) {
		return false, errors.New("x509: certificate has expired")
	}
	containerHasK3sProcessFn = func(context.Context, string) (bool, error) { return false, nil }
	inspectDockerK3dNodeFn = func(context.Context, string) (dockerK3dNodeInspect, error) {
		return dockerK3dNodeInspect{}, nil
	}
	dockerLogsSinceStartFn = func(context.Context, string, string) (string, error) {
		return "some unrelated shutdown", nil
	}

	err := ensureRunningK3dClusterHealthy(t.Context(), ClusterEntity{Name: "control-plane"})
	if err == nil {
		t.Fatal("an unrecognized k3s exit was accepted as healthy")
	}
	if !strings.Contains(err.Error(), "x509: certificate has expired") {
		t.Fatalf("error dropped the probe reason: %v", err)
	}
}

// TestWaitForK3sProcessToleratesTheStartupWindow pins that a node sampled
// moments after a restart is not mistaken for a dead one. k3d's entrypoint
// does its own setup before it execs k3s, so absence is only conclusive once
// it has persisted — a single sample would send a healthy, still-starting node
// down the log-diagnosis path, where it finds no failure signature (nothing
// failed) and aborts with a misleading error.
func TestWaitForK3sProcessToleratesTheStartupWindow(t *testing.T) {
	origProcess := containerHasK3sProcessFn
	origGrace := k3sProcessGrace
	origPoll := k3sProcessPollInterval
	t.Cleanup(func() {
		containerHasK3sProcessFn = origProcess
		k3sProcessGrace = origGrace
		k3sProcessPollInterval = origPoll
	})
	k3sProcessGrace = time.Second
	k3sProcessPollInterval = time.Millisecond

	calls := 0
	containerHasK3sProcessFn = func(context.Context, string) (bool, error) {
		calls++
		return calls >= 3, nil // absent, absent, then k3s appears
	}
	running, err := waitForK3sProcess(t.Context(), "k3d-cp-daemon-server-0")
	if err != nil {
		t.Fatalf("waitForK3sProcess: %v", err)
	}
	if !running {
		t.Fatalf("a node whose k3s appeared after %d samples was reported as exited", calls)
	}
}

// TestWaitForK3sProcessReportsASustainedAbsence keeps the drift diagnosis
// reachable: k3s that never appears within the grace IS exited, and the caller
// must go on to inspect the logs and repair.
func TestWaitForK3sProcessReportsASustainedAbsence(t *testing.T) {
	origProcess := containerHasK3sProcessFn
	origGrace := k3sProcessGrace
	origPoll := k3sProcessPollInterval
	t.Cleanup(func() {
		containerHasK3sProcessFn = origProcess
		k3sProcessGrace = origGrace
		k3sProcessPollInterval = origPoll
	})
	k3sProcessGrace = 20 * time.Millisecond
	k3sProcessPollInterval = time.Millisecond

	containerHasK3sProcessFn = func(context.Context, string) (bool, error) { return false, nil }
	running, err := waitForK3sProcess(t.Context(), "k3d-cp-daemon-server-0")
	if err != nil {
		t.Fatalf("waitForK3sProcess: %v", err)
	}
	if running {
		t.Fatal("a node with no k3s at all was reported as running")
	}
}
