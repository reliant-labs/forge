package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func fluxKubeconfigEntities() *KCLEntities {
	return &KCLEntities{
		Clusters: []ClusterEntity{{Name: "cp-daemon-e2e", Network: "net", Context: "k3d-cp-daemon-e2e"}, {Name: "control-plane-e2e", Network: "net", Context: "k3d-control-plane-e2e"}},
		KubeconfigSecrets: []KubeconfigSecretEntity{
			{Name: "cp-daemon-kubeconfig", InCluster: "k3d-control-plane-e2e", TargetCluster: "cp-daemon-e2e"},
			{Name: "elsewhere", InCluster: "gke_not_declared", TargetCluster: "x"},
		},
	}
}

func TestFluxKubeconfigSecretsSelectsDeclaredContextsOnly(t *testing.T) {
	e := fluxKubeconfigEntities()
	got := fluxKubeconfigSecrets(e)
	for _, k := range got {
		if k.Name == "elsewhere" {
			t.Fatalf("a secret for a cluster with no context must not be minted here: %+v", got)
		}
	}
	var out bytes.Buffer
	printFluxKubeconfigPlan(&out, "e2e", got)
	if len(got) != 1 || got[0].Name != "cp-daemon-kubeconfig" {
		t.Fatalf("want exactly the declared-context secret, got %+v", got)
	}
	if !strings.Contains(out.String(), "cp-daemon-kubeconfig") {
		t.Errorf("plan omits the secret:\n%s", out.String())
	}
	if fluxKubeconfigSecrets(nil) != nil {
		t.Error("nil entities must yield nothing")
	}
}

// The mint writes under the forge-secrets field manager via the Flux seam.
func TestApplyKubeconfigSecretUsesFluxManager(t *testing.T) {
	prev := fluxSecretApply
	var gotCtx, gotNS, gotMgr string
	fluxSecretApply = func(_ context.Context, kctx, ns, mgr, _ string) error {
		gotCtx, gotNS, gotMgr = kctx, ns, mgr
		return nil
	}
	t.Cleanup(func() { fluxSecretApply = prev })
	if err := applyKubeconfigSecret(context.Background(), "k3d-a", "ns", fluxSecretFieldManager, "x"); err != nil {
		t.Fatal(err)
	}
	if gotCtx != "k3d-a" || gotNS != "ns" || gotMgr != "forge-secrets" {
		t.Errorf("got %q %q %q", gotCtx, gotNS, gotMgr)
	}
}
