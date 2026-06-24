package cli

import (
	"testing"
)

const sampleClustersJSON = `{
  "clusters": [
    {"name": "cp", "servers": 1, "agents": 0, "api_port": 6443},
    {"name": "workload", "network": "k3d-cp", "registry_mirror": "inherit", "servers": 1, "agents": 2, "api_port": 6444},
    {"name": "configured", "config": "deploy/k3d.workload.yaml", "servers": 1, "agents": 0}
  ],
  "services": []
}`

// TestParseKCLEntities_Clusters pins that the declared clusters block
// parses into ClusterEntity in order, with the implicit-ownership fields
// (network / registry_mirror) and api_port preserved.
func TestParseKCLEntities_Clusters(t *testing.T) {
	entities, err := parseKCLEntities([]byte(sampleClustersJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if len(entities.Clusters) != 3 {
		t.Fatalf("clusters: got %d want 3", len(entities.Clusters))
	}

	owner := entities.Clusters[0]
	if owner.Name != "cp" {
		t.Errorf("clusters[0].Name = %q want cp", owner.Name)
	}
	if owner.Network != "" || owner.RegistryMirror != "" {
		t.Errorf("owner cluster must declare no network/registry_mirror; got network=%q mirror=%q",
			owner.Network, owner.RegistryMirror)
	}
	if owner.APIPort != 6443 {
		t.Errorf("clusters[0].APIPort = %d want 6443", owner.APIPort)
	}

	sec := entities.Clusters[1]
	if sec.Network != "k3d-cp" {
		t.Errorf("secondary.Network = %q want k3d-cp (implicit owner)", sec.Network)
	}
	if sec.RegistryMirror != "inherit" {
		t.Errorf("secondary.RegistryMirror = %q want inherit", sec.RegistryMirror)
	}
	if sec.Agents != 2 {
		t.Errorf("secondary.Agents = %d want 2", sec.Agents)
	}

	cfg := entities.Clusters[2]
	if cfg.Config != "deploy/k3d.workload.yaml" {
		t.Errorf("clusters[2].Config = %q want deploy/k3d.workload.yaml", cfg.Config)
	}
}

// TestParseKCLEntities_NoClusters confirms an env that declares no
// clusters parses to an empty list (no-op reconcile) — preserving the
// no-ensure behavior for single-cluster / cluster-less envs.
func TestParseKCLEntities_NoClusters(t *testing.T) {
	entities, err := parseKCLEntities([]byte(sampleKCLJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if len(entities.Clusters) != 0 {
		t.Errorf("clusters: got %d want 0", len(entities.Clusters))
	}
}

// TestReconcileDeclaredClusters_EmptyIsNoop guards the fast path: a nil
// cluster list never shells out to k3d, so it's safe to call
// unconditionally at the head of every `forge up` / `forge deploy`.
func TestReconcileDeclaredClusters_EmptyIsNoop(t *testing.T) {
	if err := reconcileDeclaredClusters(t.Context(), nil); err != nil {
		t.Errorf("empty reconcile should be a no-op, got %v", err)
	}
	if err := reconcileDeclaredClusters(t.Context(), []ClusterEntity{}); err != nil {
		t.Errorf("empty-slice reconcile should be a no-op, got %v", err)
	}
}

// TestParseKCLEntities_ServiceImageTagPin pins that a per-service
// image_tag round-trips through the entity parse (the JSON contract
// carries it; the KCL render layer uses it to stamp the image ref).
func TestParseKCLEntities_ServiceImageTagPin(t *testing.T) {
	const js = `{"services":[
      {"name":"reliant","image":"reliant","image_tag":"v1.4.2","deploy":{"type":"cluster","cluster":"k3d-dev","namespace":"dev","registry":"localhost:5050"}},
      {"name":"api","image":"api","deploy":{"type":"cluster","cluster":"k3d-dev","namespace":"dev","registry":"localhost:5050"}}
    ]}`
	entities, err := parseKCLEntities([]byte(js))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if got := entities.FindService("reliant").ImageTag; got != "v1.4.2" {
		t.Errorf("reliant.ImageTag = %q want v1.4.2", got)
	}
	if got := entities.FindService("api").ImageTag; got != "" {
		t.Errorf("api.ImageTag = %q want empty (env-wide tag)", got)
	}
}

// TestEffectiveServers defaults a zero/negative Servers to 1 (the schema
// default; belt-and-suspenders for a hand-built entity).
func TestEffectiveServers(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 1},
		{-3, 1},
		{1, 1},
		{3, 3},
	}
	for _, tc := range cases {
		if got := effectiveServers(ClusterEntity{Servers: tc.in}); got != tc.want {
			t.Errorf("effectiveServers(%d) = %d want %d", tc.in, got, tc.want)
		}
	}
}
