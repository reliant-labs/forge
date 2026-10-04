package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

const secretStream = `# cluster: k3d-a
apiVersion: v1
kind: Secret
metadata:
  name: raw-creds
  namespace: app
stringData:
  token: hunter2-canary
---
# cluster: k3d-a
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
  namespace: app
`

// The names a bundle records are what the deploy syncs, and no value is among
// them.
func TestEnvSyncedSecretRefsNamesTheRenderedSecrets(t *testing.T) {
	refs, err := envSyncedSecretRefs(&KCLEntities{}, nil, "app", secretStream)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != (release.BundleSecretRef{Cluster: "k3d-a", Namespace: "app", Name: "raw-creds"}) {
		t.Fatalf("refs = %+v", refs)
	}
}

// syncFluxSecrets writes one apply per (cluster, namespace) under the
// forge-secrets field manager, prints a count per cluster, and never a value.
func TestSyncFluxSecretsAppliesUnderTheFieldManagerAndPrintsNoValue(t *testing.T) {
	var gotCluster, gotNamespace, gotManager, gotStream string
	prevApply, prevNS := fluxSecretApply, fluxSecretNamespaceEnsure
	fluxSecretApply = func(_ context.Context, kctx, namespace, manager, manifests string) error {
		gotCluster, gotNamespace, gotManager, gotStream = kctx, namespace, manager, manifests
		return nil
	}
	fluxSecretNamespaceEnsure = func(context.Context, string, string) error { return nil }
	t.Cleanup(func() { fluxSecretApply, fluxSecretNamespaceEnsure = prevApply, prevNS })

	set := fluxSecretSet{}
	set.add("k3d-a", "app", map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": "raw-creds", "namespace": "app"},
		"stringData": map[string]any{"token": "hunter2-canary"},
	})
	var out bytes.Buffer
	if err := syncFluxSecrets(context.Background(), "dev-k8s", set, &out); err != nil {
		t.Fatal(err)
	}
	if gotCluster != "k3d-a" || gotNamespace != "app" || gotManager != "forge-secrets" {
		t.Errorf("apply(%q, %q, %q)", gotCluster, gotNamespace, gotManager)
	}
	if !strings.Contains(gotStream, "hunter2-canary") {
		t.Error("the value must reach the cluster")
	}
	if strings.Contains(out.String(), "hunter2-canary") {
		t.Errorf("a value was printed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "k3d-a: synced 1 secret(s)") {
		t.Errorf("no per-cluster count line:\n%s", out.String())
	}
}

// A Secret with nowhere to land is refused, never applied to the current
// context.
func TestSyncFluxSecretsRefusesAnEmptyCluster(t *testing.T) {
	set := fluxSecretSet{}
	set.add("", "app", map[string]any{"metadata": map[string]any{"name": "x"}})
	err := syncFluxSecrets(context.Background(), "dev-k8s", set, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no cluster") {
		t.Fatalf("want a no-cluster refusal, got %v", err)
	}
}

// The plan lists names only.
func TestPrintFluxSecretPlanListsNamesOnly(t *testing.T) {
	var out bytes.Buffer
	printFluxSecretPlan(&out, "dev-k8s", []release.BundleSecretRef{
		{Cluster: "k3d-a", Namespace: "app", Name: "raw-creds"},
		{Namespace: "app", Name: "all-clusters"},
	}, []string{"k3d-a", "k3d-b"})
	for _, want := range []string{"k3d-a: app/raw-creds", "k3d-a: app/all-clusters", "k3d-b: app/all-clusters"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan missing %q:\n%s", want, out.String())
		}
	}
}

// A hub-reconciled env that needs a forge-supplied value is refused, naming
// the command that fills the gap; one with nothing to sync is untouched.
func TestRefuseUnsyncableHostedSecrets(t *testing.T) {
	if err := refuseUnsyncableHostedSecrets("prod", nil); err != nil {
		t.Fatalf("no secrets, no refusal: %v", err)
	}
	err := refuseUnsyncableHostedSecrets("prod", []release.BundleSecretRef{{Namespace: "app", Name: "creds"}})
	if err == nil || !strings.Contains(err.Error(), "forge env secrets sync prod") || !strings.Contains(err.Error(), "app/creds") {
		t.Fatalf("want a refusal naming the secret and `forge env secrets sync prod`, got %v", err)
	}
}
