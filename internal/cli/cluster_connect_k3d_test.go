package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClusterConnect_TokenAgainstARealCluster drives `connect --auth token`
// and `disconnect` against a REAL throwaway k3d cluster and a fake control
// plane: the ServiceAccount, its token Secret and the RBAC are created for
// real, the minted token is read back from the cluster and sent write-only, and
// disconnect removes exactly what forge created.
//
// WHAT IS STUBBED, AND WHY ONLY THAT. A k3d API server is loopback, which the
// command refuses on purpose (the hub dials from a pod, where loopback is that
// pod). So the one seam replaced is the kubeconfig ADDRESS read; everything the
// test exists to prove — the apply, the asynchronous token population, the
// write-only send, the by-name cleanup — runs for real.
//
// Gated by testing.Short(): it creates a cluster. The cluster is named
// uniquely and deleted by that exact name; no other cluster is read or
// touched, and the developer's kubeconfig is never modified.
func TestClusterConnect_TokenAgainstARealCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("creates a real k3d cluster")
	}
	for _, bin := range []string{"k3d", "kubectl", "docker"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	name := fmt.Sprintf("fcc-%d", time.Now().UnixNano()%1_000_000_000)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	kctx := "k3d-" + name

	runCluster(ctx, t, nil, "k3d", "cluster", "create", name,
		"--servers", "1", "--agents", "0", "--no-lb",
		"--wait", "--timeout", "300s",
		"--kubeconfig-update-default=false", "--kubeconfig-switch-context=false")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		// By exact name. Best-effort, but a leak is reported.
		if out, err := exec.CommandContext(cleanup, "k3d", "cluster", "delete", name).CombinedOutput(); err != nil {
			t.Logf("cleanup: k3d cluster delete %s: %v\n%s", name, err, out)
		}
	})
	runCluster(ctx, t, nil, "k3d", "kubeconfig", "write", name,
		"--output", kubeconfig, "--overwrite", "--kubeconfig-switch-context=false")

	// A private kubeconfig for every kubectl forge shells out to.
	t.Setenv("KUBECONFIG", kubeconfig)

	restoreKube, restoreClient := kubeconfigClusterOf, clusterConnectClient
	t.Cleanup(func() { kubeconfigClusterOf, clusterConnectClient = restoreKube, restoreClient })
	kubeconfigClusterOf = func(string) (string, string, error) {
		return "https://203.0.113.10:6443", "ca-bundle", nil
	}
	caller := &fakeConnectCaller{}
	clusterConnectClient = func(context.Context, string, string) (cloudCaller, string, error) {
		return caller, "acme", nil
	}

	var out strings.Builder
	if err := runClusterConnect(ctx, clusterConnectOptions{
		Name: "e2e", KubeContext: kctx, Auth: "token", Env: "prod", Out: &out,
	}); err != nil {
		t.Fatalf("connect: %v\n%s", err, out.String())
	}

	// The objects exist in the cluster.
	for _, obj := range [][]string{
		{"get", "namespace", connectTokenNamespace},
		{"get", "serviceaccount", connectTokenServiceAccount, "-n", connectTokenNamespace},
		{"get", "secret", connectTokenServiceAccount + "-token", "-n", connectTokenNamespace},
		{"get", "clusterrole", "forge-connect-e2e"},
		{"get", "clusterrolebinding", "forge-connect-e2e"},
	} {
		runCluster(ctx, t, nil, "kubectl", append([]string{"--context", kctx}, obj...)...)
	}

	// The token the control plane received is the one the cluster minted: a
	// service-account JWT, sent write-only.
	sent, _ := caller.requests[procConnectCluster]["token"].(string)
	if !strings.HasPrefix(sent, "eyJ") {
		t.Fatalf("the token sent to the control plane is not a service-account JWT: %q", sent)
	}
	if strings.Contains(out.String(), sent) {
		t.Fatal("the minted token appears in the command's output")
	}
	if caller.requests[procConnectCluster]["auth"] != authToken {
		t.Errorf("auth = %v, want %s", caller.requests[procConnectCluster]["auth"], authToken)
	}

	// Disconnect: revoke, then delete by exact name.
	caller.clusters = []wireConnectedCluster{{ID: "cl_new", Name: "e2e"}}
	out.Reset()
	if err := runClusterDisconnect(ctx, "e2e", kctx, "prod", "", &out); err != nil {
		t.Fatalf("disconnect: %v\n%s", err, out.String())
	}
	if _, ok := caller.requests[procRemoveCluster]; !ok {
		t.Error("disconnect never called RemoveCluster")
	}
	for _, obj := range [][]string{
		{"get", "serviceaccount", connectTokenServiceAccount, "-n", connectTokenNamespace},
		{"get", "secret", connectTokenServiceAccount + "-token", "-n", connectTokenNamespace},
		{"get", "clusterrole", "forge-connect-e2e"},
		{"get", "clusterrolebinding", "forge-connect-e2e"},
	} {
		if err := tryCluster(ctx, "kubectl", append([]string{"--context", kctx}, obj...)...); err == nil {
			t.Errorf("disconnect left %s behind", strings.Join(obj[1:], " "))
		}
	}
	// The shared namespace is deliberately left in place.
	runCluster(ctx, t, nil, "kubectl", "--context", kctx, "get", "namespace", connectTokenNamespace)

	// A second disconnect converges rather than failing on missing objects.
	out.Reset()
	if err := runClusterDisconnect(ctx, "e2e", kctx, "prod", "", &out); err != nil {
		t.Fatalf("a repeated disconnect failed: %v\n%s", err, out.String())
	}

	if _, err := os.Stat(kubeconfig); err != nil {
		t.Errorf("private kubeconfig vanished: %v", err)
	}
}

func runCluster(ctx context.Context, t *testing.T, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func tryCluster(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}
