package cli

import (
	"context"
	"testing"
)

const sampleClustersJSON = `{
  "clusters": [
    {"name": "cp", "context": "k3d-cp", "registry_inherit": false, "servers": 1, "agents": 0, "api_port": 6443},
    {"name": "workload", "context": "k3d-workload", "network": "k3d-cp", "registry_inherit": true, "servers": 1, "agents": 2, "api_port": 6444},
    {"name": "configured", "context": "k3d-configured", "config": "deploy/k3d.workload.yaml", "servers": 1, "agents": 0}
  ],
  "services": []
}`

// TestParseKCLEntities_Clusters pins that the declared clusters block
// parses into ClusterEntity in order, with the derived-ownership fields
// (context / network / registry_inherit) and api_port preserved.
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
	if owner.Context != "k3d-cp" {
		t.Errorf("clusters[0].Context = %q want k3d-cp", owner.Context)
	}
	if owner.Network != "" || owner.RegistryInherit {
		t.Errorf("owner cluster must derive no network/registry_inherit; got network=%q inherit=%v",
			owner.Network, owner.RegistryInherit)
	}
	if owner.APIPort != 6443 {
		t.Errorf("clusters[0].APIPort = %d want 6443", owner.APIPort)
	}

	sec := entities.Clusters[1]
	if sec.Network != "k3d-cp" {
		t.Errorf("secondary.Network = %q want k3d-cp (derived from owner)", sec.Network)
	}
	if !sec.RegistryInherit {
		t.Errorf("secondary.RegistryInherit = %v want true", sec.RegistryInherit)
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
	if err := reconcileDeclaredClusters(t.Context(), nil, "", ""); err != nil {
		t.Errorf("empty reconcile should be a no-op, got %v", err)
	}
	if err := reconcileDeclaredClusters(t.Context(), []ClusterEntity{}, "", ""); err != nil {
		t.Errorf("empty-slice reconcile should be a no-op, got %v", err)
	}
}

// TestReconcileDeclaredClusters_NoImperativeIngress guards the
// declarative-ingress invariant: reconcile NEVER installs the Gateway API
// stack imperatively, even for a cluster that still carries the legacy
// `Ingress` flag. Ingress is a DECLARED forge.HelmChart applied by the
// deploy phase (`forge deploy <env> --target=envoy-gateway`), so the
// per-cluster ingress-install seam is gone entirely. A warm reconcile of an
// already-existing cluster is a pure no-op beyond the secondary-node setup.
// The clusterExists seam is stubbed so the test never touches k3d/kubectl;
// the absence of any ingress shell-out is the assertion (the test would not
// compile if an installClusterIngressFn seam were still referenced).
func TestReconcileDeclaredClusters_NoImperativeIngress(t *testing.T) {
	origExists := clusterExistsFn
	origSetup := setupSecondaryClusterNodeFn
	t.Cleanup(func() {
		clusterExistsFn = origExists
		setupSecondaryClusterNodeFn = origSetup
	})

	clusterExistsFn = func(_ context.Context, _ string) (bool, error) { return true, nil }
	// A nested-secondary setup seam is the ONLY warm-path side effect that
	// remains; an owner cluster (no derived network/inherit) triggers none.
	setupSecondaryClusterNodeFn = func(_ context.Context, _ ClusterEntity) error { return nil }

	// `Ingress: true` is deliberately set to prove it is now inert — no
	// install is triggered. A warm reconcile must succeed as a no-op.
	clusters := []ClusterEntity{
		{Name: "control-plane", Ingress: true, HostPorts: true},
		{Name: "cp-daemon", Ingress: false},
	}
	if err := reconcileDeclaredClusters(t.Context(), clusters, "proj", "e2e"); err != nil {
		t.Fatalf("reconcileDeclaredClusters: %v", err)
	}
}

// TestIsNestedSecondary pins the gate that identifies a secondary cluster
// nested on an owner's docker network: an `owner` reference, which the
// render layer projects as RegistryInherit=true + a derived Network. An
// owner cluster derives neither; a malformed half-projection (only one of
// the two) is not treated as nested.
func TestIsNestedSecondary(t *testing.T) {
	cases := []struct {
		name string
		c    ClusterEntity
		want bool
	}{
		{"owner (neither)", ClusterEntity{Name: "cp"}, false},
		{"nested secondary", ClusterEntity{Name: "wl", Network: "k3d-cp", RegistryInherit: true}, true},
		{"network only", ClusterEntity{Name: "wl", Network: "k3d-cp"}, false},
		{"inherit only", ClusterEntity{Name: "wl", RegistryInherit: true}, false},
	}
	for _, tc := range cases {
		if got := isNestedSecondary(tc.c); got != tc.want {
			t.Errorf("%s: isNestedSecondary = %v want %v", tc.name, got, tc.want)
		}
	}
}

// TestReconcileDeclaredClusters_SecondarySetup asserts the secondary-node
// setup is invoked for EXACTLY the nested-secondary clusters (derived
// network + registry_inherit) and skipped for standalone owners. All
// shell-out seams are stubbed so the test never touches k3d/docker/kubectl;
// clusters report as already existing so the create branch is never
// reached (the warm path also gates the secondary setup on the same
// predicate, so this exercises that path).
func TestReconcileDeclaredClusters_SecondarySetup(t *testing.T) {
	origExists := clusterExistsFn
	origSetup := setupSecondaryClusterNodeFn
	t.Cleanup(func() {
		clusterExistsFn = origExists
		setupSecondaryClusterNodeFn = origSetup
	})

	clusterExistsFn = func(_ context.Context, _ string) (bool, error) { return true, nil }

	var setup []string
	setupSecondaryClusterNodeFn = func(_ context.Context, c ClusterEntity) error {
		setup = append(setup, c.Name)
		return nil
	}

	clusters := []ClusterEntity{
		{Name: "control-plane"}, // standalone owner — no setup
		{Name: "cp-daemon", Network: "k3d-control-plane", RegistryInherit: true}, // nested — setup
	}
	if err := reconcileDeclaredClusters(t.Context(), clusters, "", ""); err != nil {
		t.Fatalf("reconcileDeclaredClusters: %v", err)
	}

	if len(setup) != 1 || setup[0] != "cp-daemon" {
		t.Fatalf("secondary setup invoked for %v; want exactly [cp-daemon]", setup)
	}
}

// TestNodeHostsLineFor checks the host-gateway alias is matched on a
// whole whitespace field (not a substring), and returns the full line.
func TestNodeHostsLineFor(t *testing.T) {
	const hosts = "10.0.0.1 k3d-cp-server-0\n192.168.65.254 host.k3d.internal\n"
	if got := nodeHostsLineFor(hosts, "host.k3d.internal"); got != "192.168.65.254 host.k3d.internal" {
		t.Errorf("nodeHostsLineFor = %q want the host.k3d.internal line", got)
	}
	if got := nodeHostsLineFor(hosts, "missing.alias"); got != "" {
		t.Errorf("nodeHostsLineFor(missing) = %q want empty", got)
	}
	// A longer hostname that merely contains the alias as a substring must
	// not false-match.
	if got := nodeHostsLineFor("10.0.0.2 host.k3d.internal.evil\n", "host.k3d.internal"); got != "" {
		t.Errorf("nodeHostsLineFor(substring) = %q want empty (no substring match)", got)
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
