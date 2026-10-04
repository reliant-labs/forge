package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Opt-in: FORGE_LIVE_FLUX_KUBECONFIG="<consumer-ctx>,<target-k3d-cluster>,<docker-network>".
// Mints through the Flux-path seam (real ApplySecrets, field manager
// forge-secrets) and mounts the Secret in a pod.
func TestFluxKubeconfigMint_Live(t *testing.T) {
	spec := os.Getenv("FORGE_LIVE_FLUX_KUBECONFIG")
	parts := strings.Split(spec, ",")
	if len(parts) != 3 {
		t.Skip("set FORGE_LIVE_FLUX_KUBECONFIG=<consumer-ctx>,<target-cluster>,<network>")
	}
	kctx, target, network := parts[0], parts[1], parts[2]
	const ns = "fkc-live"
	t.Cleanup(func() { _ = exec.Command("kubectl", "--context", kctx, "delete", "ns", ns, "--wait=false").Run() })

	secrets := []KubeconfigSecretEntity{{
		Name: "cross-kubeconfig", InCluster: kctx, TargetCluster: target,
		ContextName: "target", Namespace: ns, Reachability: "in-network",
	}}
	if err := mintKubeconfigSecretsAs(t.Context(), secrets, network, ns, fluxSecretFieldManager, false); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("kubectl", "--context", kctx, "-n", ns, "get", "secret", "cross-kubeconfig",
		"--show-managed-fields", "-o", "yaml").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "forge-secrets") {
		t.Fatalf("secret missing or wrong manager: %v\n%s", err, out)
	}
	pod := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"mounter","namespace":"` + ns + `"},"spec":{"restartPolicy":"Never","containers":[{"name":"c","image":"busybox","command":["sh","-c","test -s /kc/kubeconfig && sleep 3"],"volumeMounts":[{"name":"kc","mountPath":"/kc"}]}],"volumes":[{"name":"kc","secret":{"secretName":"cross-kubeconfig"}}]}}`
	cmd := exec.Command("kubectl", "--context", kctx, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(pod)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, o)
	}
	if o, err := exec.Command("kubectl", "--context", kctx, "-n", ns, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/mounter", "--timeout=120s").CombinedOutput(); err != nil {
		t.Fatalf("pod did not start/succeed: %v\n%s", err, o)
	}
}
