package cli

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestParseKCLEntities_KubeconfigSecrets pins that the declared
// kubeconfig_secrets block parses into KubeconfigSecretEntity with
// defaults preserved as the renderer emits them.
func TestParseKCLEntities_KubeconfigSecrets(t *testing.T) {
	const js = `{"kubeconfig_secrets":[
      {"name":"workload-kubeconfig","in_cluster":"k3d-cp","target_cluster":"workload","context_name":"workload","key":"kubeconfig","reachability":"in-network"},
      {"name":"prod-kubeconfig","in_cluster":"k3d-cp","target_cluster":"prod-workload","context_name":"prod-workload","key":"config","namespace":"system","reachability":"endpoint"}
    ],"services":[]}`
	entities, err := parseKCLEntities([]byte(js))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if len(entities.KubeconfigSecrets) != 2 {
		t.Fatalf("kubeconfig_secrets: got %d want 2", len(entities.KubeconfigSecrets))
	}
	k0 := entities.KubeconfigSecrets[0]
	if k0.InCluster != "k3d-cp" || k0.TargetCluster != "workload" || k0.ContextName != "workload" {
		t.Errorf("k0 fields wrong: %+v", k0)
	}
	if k0.Reachability != "in-network" {
		t.Errorf("k0.Reachability = %q want in-network", k0.Reachability)
	}
	k1 := entities.KubeconfigSecrets[1]
	if k1.Namespace != "system" || k1.Key != "config" || k1.Reachability != "endpoint" {
		t.Errorf("k1 fields wrong: %+v", k1)
	}
}

// TestMintKubeconfigSecrets_EmptyIsNoop confirms the mint phase is a
// no-op for an env declaring no kubeconfig secrets (never shells out).
func TestMintKubeconfigSecrets_EmptyIsNoop(t *testing.T) {
	if err := mintKubeconfigSecrets(t.Context(), nil, "k3d-cp", "dev"); err != nil {
		t.Errorf("empty mint should be a no-op, got %v", err)
	}
}

// TestOwnerNetworkFromClusters covers the implicit-ownership network
// resolution: the first cluster that DECLARES a network (a secondary
// pointing at the owner) is the shared network; absent that, a lone
// cluster's own k3d network; otherwise empty (no cross-cluster wiring).
// There is no "primary" notion — the value is exactly what the clusters
// declare.
func TestOwnerNetworkFromClusters(t *testing.T) {
	cases := []struct {
		name string
		in   []ClusterEntity
		want string
	}{
		{
			name: "secondary declares owner network",
			in: []ClusterEntity{
				{Name: "cp"},
				{Name: "workload", Network: "k3d-cp", RegistryInherit: true},
			},
			want: "k3d-cp",
		},
		{
			name: "lone cluster falls back to its own network",
			in:   []ClusterEntity{{Name: "dev"}},
			want: "k3d-dev",
		},
		{
			name: "multi-cluster with no declared network => empty",
			in:   []ClusterEntity{{Name: "a"}, {Name: "b"}},
			want: "",
		},
		{
			name: "no clusters => empty",
			in:   nil,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownerNetworkFromClusters(tc.in); got != tc.want {
				t.Errorf("ownerNetworkFromClusters = %q want %q", got, tc.want)
			}
		})
	}
}

// TestKubeconfigSecretYAML pins the minted Secret manifest shape: a
// base64 `data` entry under the declared key, namespace + name, and the
// forge managed-by label.
func TestKubeconfigSecretYAML(t *testing.T) {
	payload := []byte("apiVersion: v1\nkind: Config\n")
	got := kubeconfigSecretYAML("workload-kubeconfig", "system", "config", payload)

	for _, want := range []string{
		"kind: Secret",
		"name: workload-kubeconfig",
		"namespace: system",
		"app.kubernetes.io/managed-by: forge",
		"type: Opaque",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Secret YAML missing %q:\n%s", want, got)
		}
	}
	// The kubeconfig must be base64-encoded under the declared key.
	wantData := "config: " + base64.StdEncoding.EncodeToString(payload)
	if !strings.Contains(got, wantData) {
		t.Errorf("Secret YAML missing base64 data %q:\n%s", wantData, got)
	}
	// Never inline the raw kubeconfig (it would be stringData, not data).
	if strings.Contains(got, "stringData") {
		t.Errorf("kubeconfig Secret should use base64 data, not stringData:\n%s", got)
	}
}
