package deploytarget

import (
	"context"
	"strings"
	"testing"
)

// TestRollbackContext_DeclaredIsTheOnlySource confirms the K8sCluster
// rollback derives its kubectl context SOLELY from the group's declared
// cluster (KCL forge.K8sCluster.cluster) — the same declarative binding
// the deploy path uses, with no CLI override. A `forge deploy <env>
// --rollback` therefore can't `kubectl rollout undo` against the wrong
// cluster by relying on the active context.
func TestRollbackContext_DeclaredIsTheOnlySource(t *testing.T) {
	p := K8sClusterProvider{}
	g := ServiceGroup{
		ProviderID: "k8s-cluster",
		Cluster:    "gke_reliant-labs-475814_us-central1_prod",
		Namespace:  "cp-forge-prod",
	}
	if got := p.rollbackContext(g); got != g.Cluster {
		t.Errorf("rollback context should be the declared cluster: want %q, got %q", g.Cluster, got)
	}
}

// TestRollbackContext_NoClusterEmpty confirms a group with no declared
// cluster yields an empty context. Empty is NOT a silent fall-back to the
// active context — Rollback refuses an empty context (see
// TestRollback_RefusesEmptyContext).
func TestRollbackContext_NoClusterEmpty(t *testing.T) {
	p := K8sClusterProvider{}
	g := ServiceGroup{ProviderID: "k8s-cluster", Namespace: "ns"}
	if got := p.rollbackContext(g); got != "" {
		t.Errorf("no declared cluster should yield empty context, got %q", got)
	}
}

// TestRollback_RefusesEmptyContext is the footgun guard on the rollback
// WRITE path, mirroring cluster.KubectlApply: `kubectl rollout undo` is a
// cluster mutation, so a group that carries no declared cluster must
// refuse BEFORE shelling kubectl rather than rolling back against
// whatever context is currently active. The guard returns before exec, so
// this needs no cluster.
func TestRollback_RefusesEmptyContext(t *testing.T) {
	p := K8sClusterProvider{}
	g := ServiceGroup{
		ProviderID: "k8s-cluster",
		Namespace:  "ns",
		Services:   []ResolvedService{{Name: "api"}},
	}
	err := p.Rollback(context.Background(), g, "")
	if err == nil {
		t.Fatal("Rollback with empty context = nil, want error (must not fall back to current-context)")
	}
	if !strings.Contains(err.Error(), "without an explicit kubectl context") {
		t.Errorf("Rollback error = %v, want the declarative-context refusal", err)
	}
}
