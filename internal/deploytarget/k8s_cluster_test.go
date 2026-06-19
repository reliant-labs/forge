package deploytarget

import "testing"

// TestRollbackContext_DeclaredIsDefault confirms the K8sCluster
// rollback derives its kubectl context from the group's DECLARED cluster
// (KCL forge.K8sCluster.cluster) when no explicit override is set on the
// provider — the same declarative binding the deploy path uses, so a
// `forge deploy <env> --rollback` can't `kubectl rollout undo` against
// the wrong cluster by relying on the active context.
func TestRollbackContext_DeclaredIsDefault(t *testing.T) {
	p := K8sClusterProvider{} // no override
	g := ServiceGroup{
		ProviderID: "k8s-cluster",
		Cluster:    "gke_reliant-labs-475814_us-central1_prod",
		Namespace:  "cp-forge-prod",
	}
	if got := p.rollbackContext(g); got != g.Cluster {
		t.Errorf("rollback context should be the declared cluster: want %q, got %q", g.Cluster, got)
	}
}

// TestRollbackContext_OverrideWins confirms the explicit provider-level
// override (the deploy `--context` flag) replaces the declared cluster.
func TestRollbackContext_OverrideWins(t *testing.T) {
	p := K8sClusterProvider{Context: "override-ctx"}
	g := ServiceGroup{ProviderID: "k8s-cluster", Cluster: "gke_declared", Namespace: "ns"}
	if got := p.rollbackContext(g); got != "override-ctx" {
		t.Errorf("override should win: want %q, got %q", "override-ctx", got)
	}
}

// TestRollbackContext_NoClusterEmpty confirms a group with no declared
// cluster yields an empty context (= kubectl's current context).
func TestRollbackContext_NoClusterEmpty(t *testing.T) {
	p := K8sClusterProvider{}
	g := ServiceGroup{ProviderID: "k8s-cluster", Namespace: "ns"}
	if got := p.rollbackContext(g); got != "" {
		t.Errorf("no declared cluster should yield empty context, got %q", got)
	}
}
