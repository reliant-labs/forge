package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
)

// fakeProvider is a minimal Provider used by the dispatch tests. It
// records every Deploy invocation so tests can assert the dispatcher
// invoked it on each group.
type fakeProvider struct {
	id          string
	deployCalls []deploytarget.ServiceGroup
	deployErr   error
}

func (f *fakeProvider) Name() string { return f.id }

func (f *fakeProvider) Deploy(_ context.Context, g deploytarget.ServiceGroup) error {
	f.deployCalls = append(f.deployCalls, g)
	return f.deployErr
}

// Observe satisfies the Provider interface. These tests exercise the
// DEPLOY dispatcher, which never calls it — so it declines
// rather than returning a fabricated green observation that a future
// test could accidentally assert against.
func (f *fakeProvider) Observe(_ context.Context, _ deploytarget.ServiceGroup) (deploytarget.Observed, error) {
	return deploytarget.Observed{ProviderID: f.id}, deploytarget.ObservationUnsupportedError{
		Provider: f.id,
		Reason:   "test double for the deploy dispatch; observation is not part of these tests",
	}
}

// TestBuildDeployGroupsWithOpts_DryRunPropagates confirms the
// dry-run flag is stamped onto every group built from the rendered
// KCL. This is the plumbing that ties --dry-run on the CLI to the
// per-provider dry-run gating.
func TestBuildDeployGroupsWithOpts_DryRunPropagates(t *testing.T) {
	body := `{"services":[
		{"name":"edge","image":"x/edge","deploy":{"type":"external","deploy_cmd":"flyctl deploy"}},
		{"name":"web","image":"x/web","deploy":{"type":"compose","compose_file":"docker-compose.yml"}}
	]}`
	entities, err := parseKCLEntities([]byte(body))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	groups, err := buildDeployGroupsWithOpts("prod", entities, "fallback-ns", true)
	if err != nil {
		t.Fatalf("buildDeployGroupsWithOpts: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d", len(groups))
	}
	for i, g := range groups {
		if !g.DryRun {
			t.Errorf("group[%d] (%s) DryRun: want true, got false", i, g.ProviderID)
		}
	}
}

// TestKclEntitiesHaveK8sCluster confirms the "any cluster-shaped
// service?" gate used to suppress the namespace banner and
// kubectl-context guard for external-only / compose-only projects.
func TestKclEntitiesHaveK8sCluster(t *testing.T) {
	t.Run("external-only", func(t *testing.T) {
		body := `{"services":[{"name":"edge","deploy":{"type":"external","deploy_cmd":"flyctl deploy"}}]}`
		ents, err := parseKCLEntities([]byte(body))
		if err != nil {
			t.Fatalf("parseKCLEntities: %v", err)
		}
		if kclEntitiesHaveK8sCluster(ents) {
			t.Error("external-only should report no k8s services")
		}
	})
	t.Run("cluster-shaped", func(t *testing.T) {
		body := `{"services":[{"name":"edge","deploy":{"type":"cluster","replicas":1}}]}`
		ents, err := parseKCLEntities([]byte(body))
		if err != nil {
			t.Fatalf("parseKCLEntities: %v", err)
		}
		if !kclEntitiesHaveK8sCluster(ents) {
			t.Error("cluster service should be detected")
		}
	})
	t.Run("nil entities", func(t *testing.T) {
		if kclEntitiesHaveK8sCluster(nil) {
			t.Error("nil entities should return false")
		}
	})
}

// TestDeployCmd_SkipFrontendFlagRegistered confirms `--skip-frontend`
// is declared with a help line that names the k8s-only intent — the
// GAP-2 flag that runs the k8s apply but suppresses the Frontend
// (Firebase) build+deploy dispatch.
func TestDeployCmd_SkipFrontendFlagRegistered(t *testing.T) {
	cmd := newDeployCmd()
	f := cmd.Flags().Lookup("skip-frontend")
	if f == nil {
		t.Fatal("--skip-frontend flag not registered")
	}
	if f.Value.Type() != "bool" {
		t.Errorf("--skip-frontend should be a bool flag, got %q", f.Value.Type())
	}
	if !strings.Contains(f.Usage, "Frontend") && !strings.Contains(f.Usage, "frontend") {
		t.Errorf("--skip-frontend usage should mention the frontend, got %q", f.Usage)
	}
}

// TestDispatchDeployGroups_FailureIsNotReverted: a failed group returns its
// error and nothing tries to put the previous version back — there is no
// rollback, recovery is roll forward. The dispatcher stops at the failed
// group rather than deploying the rest on top of it.
func TestDispatchDeployGroups_FailureIsNotReverted(t *testing.T) {
	failing := &fakeProvider{id: "external", deployErr: errors.New("flyctl boom")}
	after := &fakeProvider{id: "compose"}
	reg := deploytarget.NewRegistry()
	reg.Register(failing)
	reg.Register(after)
	groups := []deploytarget.ServiceGroup{
		{ProviderID: "external", Env: "prod", Services: []deploytarget.ResolvedService{{Name: "edge"}}},
		{ProviderID: "compose", Env: "prod", Services: []deploytarget.ResolvedService{{Name: "web"}}},
	}
	err := dispatchDeployGroups(context.Background(), reg, groups)
	if err == nil || !strings.Contains(err.Error(), "deploy external") || !strings.Contains(err.Error(), "flyctl boom") {
		t.Fatalf("want the provider's deploy error wrapped with its id, got %v", err)
	}
	if len(failing.deployCalls) != 1 || len(after.deployCalls) != 0 {
		t.Errorf("dispatch must stop at the failed group: failing=%d after=%d", len(failing.deployCalls), len(after.deployCalls))
	}
}

// TestDeployCmd_RollbackFlagRemoved: `forge env deploy --rollback` is gone.
// Recovery is roll forward, so the flag must be an unknown-flag error rather
// than a silently accepted no-op.
func TestDeployCmd_RollbackFlagRemoved(t *testing.T) {
	cmd := newDeployCmd()
	if cmd.Flags().Lookup("rollback") != nil {
		t.Fatal("--rollback is registered on `forge env deploy` again")
	}
	cmd.SetArgs([]string{"prod", "--rollback"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --rollback") {
		t.Fatalf("want an unknown-flag error for --rollback, got %v", err)
	}
}

// k8sGroupWithSvcs builds a k8s-cluster ServiceGroup on the given cluster
// with the named services — the input shape clusterScopeForGroups consumes.
func k8sGroupWithSvcs(cluster string, svcNames ...string) deploytarget.ServiceGroup {
	g := deploytarget.ServiceGroup{ProviderID: "k8s-cluster", Cluster: cluster, Namespace: "ns"}
	for _, n := range svcNames {
		g.Services = append(g.Services, deploytarget.ResolvedService{Name: n})
	}
	return g
}

// TestClusterScopeForGroups_SingleClusterIsNil pins the no-op invariant:
// when every k8s group declares the SAME cluster (the dev-k8s / staging /
// prod common case), there is no second cluster to isolate, so the scope
// closure returns nil and the apply path stays byte-identical to the pre-fix
// behaviour.
func TestClusterScopeForGroups_SingleClusterIsNil(t *testing.T) {
	groups := []deploytarget.ServiceGroup{
		k8sGroupWithSvcs("k3d-control-plane", "admin-server", "workspace-controller"),
	}
	scopeFor := clusterScopeForGroups(groups, nil)
	if got := scopeFor(groups[0]); got != nil {
		t.Errorf("single-cluster env must yield nil scope (no-op), got %+v", got)
	}
}

// TestClusterScopeForGroups_TwoClustersPartition is the multi-cluster
// assertion mirroring the e2e env, in the DECLARED-CLUSTER-ONLY model: the
// bulk of services on k3d-control-plane and a lone workspace-proxy on
// k3d-cp-daemon. Each group's scope must own ONLY its own services and mark
// the other cluster's services as OtherApps — there is NO primary flag (the
// most-services heuristic is gone; routing is by the manifest's owner).
func TestClusterScopeForGroups_TwoClustersPartition(t *testing.T) {
	controlPlaneG := k8sGroupWithSvcs("k3d-control-plane", "admin-server", "workspace-controller", "reliant-api-server")
	daemonG := k8sGroupWithSvcs("k3d-cp-daemon", "workspace-proxy")
	groups := []deploytarget.ServiceGroup{controlPlaneG, daemonG}
	scopeFor := clusterScopeForGroups(groups, nil)

	cs := scopeFor(controlPlaneG)
	if cs == nil {
		t.Fatal("control-plane group should get a non-nil scope in a multi-cluster env")
	}
	if _, ok := cs.OwnApps["admin-server"]; !ok {
		t.Errorf("control-plane scope must own admin-server, got %+v", cs.OwnApps)
	}
	if _, ok := cs.OtherApps["workspace-proxy"]; !ok {
		t.Errorf("control-plane scope must mark workspace-proxy as another cluster's app")
	}
	if _, leaked := cs.OwnApps["workspace-proxy"]; leaked {
		t.Errorf("control-plane scope must NOT own the daemon cluster's workspace-proxy")
	}

	ds := scopeFor(daemonG)
	if ds == nil {
		t.Fatal("daemon group should get a non-nil scope in a multi-cluster env")
	}
	if _, ok := ds.OwnApps["workspace-proxy"]; !ok {
		t.Errorf("daemon scope must own workspace-proxy, got %+v", ds.OwnApps)
	}
	if _, ok := ds.OtherApps["admin-server"]; !ok {
		t.Errorf("daemon scope must mark admin-server as another cluster's app")
	}
	if _, leaked := ds.OwnApps["admin-server"]; leaked {
		t.Errorf("daemon scope must NOT own the control-plane cluster's admin-server")
	}
}

// TestClusterScopeForGroups_InfraServiceRoutesToItsCluster pins the declared
// model's no-primary property: an IMAGE-LESS infra service (here "cp-infra",
// which owns the env-level Namespace/Gateway via forge.Service.manifests) is
// a normal cluster group member, so its name lands in OwnApps for the cluster
// it declares and OtherApps for the other — its stamped manifests route to
// THAT cluster, with no most-services guess.
func TestClusterScopeForGroups_InfraServiceRoutesToItsCluster(t *testing.T) {
	controlPlaneG := k8sGroupWithSvcs("k3d-control-plane", "admin-server", "cp-infra")
	daemonG := k8sGroupWithSvcs("k3d-cp-daemon", "workspace-proxy")
	groups := []deploytarget.ServiceGroup{controlPlaneG, daemonG}
	scopeFor := clusterScopeForGroups(groups, nil)

	cs := scopeFor(controlPlaneG)
	if _, ok := cs.OwnApps["cp-infra"]; !ok {
		t.Errorf("control-plane scope must own the infra service cp-infra, got %+v", cs.OwnApps)
	}
	ds := scopeFor(daemonG)
	if _, ok := ds.OtherApps["cp-infra"]; !ok {
		t.Errorf("daemon scope must mark cp-infra as another cluster's app (its manifests stay on control-plane)")
	}
	if _, leaked := ds.OwnApps["cp-infra"]; leaked {
		t.Errorf("daemon scope must NOT own cp-infra")
	}
}

// clusterSvcEntity builds a cluster-shaped ServiceEntity on the given cluster —
// the render-order input mainClusterForEntities walks to resolve the env's main
// cluster (the env-level deploy target an operator/cronjob lands on).
func clusterSvcEntity(name, cluster string) ServiceEntity {
	return ServiceEntity{
		Name: name,
		Deploy: DeployConfigEntity{
			Type:    "cluster",
			Cluster: &K8sCluster{Cluster: cluster, Namespace: "ns"},
		},
	}
}

// TestClusterScopeForGroups_OperatorAttributedToMainCluster is the regression
// for the manifest-scoping leak: an OPERATOR (and a CRONJOB) carry no per-service
// deploy block, so they're never part of the service-to-cluster grouping — but
// forge stamps `app.kubernetes.io/name` on their Deployment/Job/RBAC all the
// same. Before the fix the scoper saw those app labels as ungrouped and KEPT
// them on EVERY cluster, replicating a control-plane operator (workspace-
// controller) into the daemon cluster (no SA / no secret → stuck
// ContainerCreating → failed rollout). With operators/cronjobs attributed to the
// env's main cluster (the first cluster-shaped service's cluster), their app
// labels must land in the main cluster's OwnApps and the OTHER cluster's
// OtherApps — so the scoper DROPS them from the daemon cluster.
func TestClusterScopeForGroups_OperatorAttributedToMainCluster(t *testing.T) {
	// Mirror the e2e env: bulk of services + the operator + the migrate cronjob
	// on k3d-control-plane (the env's main cluster — admin-server is the first
	// cluster-shaped service), and a lone workspace-proxy on k3d-cp-daemon.
	controlPlaneG := k8sGroupWithSvcs("k3d-control-plane", "admin-server", "reliant-api-server")
	daemonG := k8sGroupWithSvcs("k3d-cp-daemon", "workspace-proxy")
	groups := []deploytarget.ServiceGroup{controlPlaneG, daemonG}
	entities := &KCLEntities{
		Services: []ServiceEntity{
			clusterSvcEntity("admin-server", "k3d-control-plane"),
			clusterSvcEntity("workspace-proxy", "k3d-cp-daemon"),
			clusterSvcEntity("reliant-api-server", "k3d-control-plane"),
		},
		Operators: []OperatorEntity{{Name: "workspace-controller"}},
		CronJobs:  []CronJobEntity{{Name: "control-plane-migrate"}},
	}
	scopeFor := clusterScopeForGroups(groups, entities)

	cs := scopeFor(controlPlaneG)
	if cs == nil {
		t.Fatal("control-plane group should get a non-nil scope in a multi-cluster env")
	}
	if _, ok := cs.OwnApps["workspace-controller"]; !ok {
		t.Errorf("control-plane scope must OWN the operator workspace-controller, got %+v", cs.OwnApps)
	}
	if _, ok := cs.OwnApps["control-plane-migrate"]; !ok {
		t.Errorf("control-plane scope must OWN the cronjob control-plane-migrate, got %+v", cs.OwnApps)
	}

	ds := scopeFor(daemonG)
	if ds == nil {
		t.Fatal("daemon group should get a non-nil scope in a multi-cluster env")
	}
	// The leak: the operator/cronjob must be in the daemon cluster's OtherApps
	// (→ DROPPED there), and NOT in its OwnApps (→ would be replicated).
	if _, ok := ds.OtherApps["workspace-controller"]; !ok {
		t.Errorf("daemon scope must mark workspace-controller as another cluster's app (dropped from cp-daemon), got OtherApps %+v", ds.OtherApps)
	}
	if _, leaked := ds.OwnApps["workspace-controller"]; leaked {
		t.Errorf("daemon scope must NOT own workspace-controller — that's the replication leak")
	}
	if _, ok := ds.OtherApps["control-plane-migrate"]; !ok {
		t.Errorf("daemon scope must mark control-plane-migrate as another cluster's app (dropped from cp-daemon)")
	}
	if _, leaked := ds.OwnApps["control-plane-migrate"]; leaked {
		t.Errorf("daemon scope must NOT own control-plane-migrate")
	}
}

// TestMainClusterForEntities_FirstClusterShapedService pins the main-cluster
// resolution: it's the FIRST cluster-shaped service's cluster in render order
// (matching firstK8sClusterField / expectedClusterForEnv), so a lone
// cross-cluster override later in the list never wins.
func TestMainClusterForEntities_FirstClusterShapedService(t *testing.T) {
	entities := &KCLEntities{
		Services: []ServiceEntity{
			clusterSvcEntity("admin-server", "k3d-control-plane"),
			clusterSvcEntity("workspace-proxy", "k3d-cp-daemon"),
		},
	}
	if got := mainClusterForEntities(entities, nil); got != "k3d-control-plane" {
		t.Errorf("main cluster should be the first cluster-shaped service's cluster, got %q", got)
	}
	// Fallback: no service carries a cluster → first k8s group's cluster.
	groups := []deploytarget.ServiceGroup{k8sGroupWithSvcs("k3d-only", "svc")}
	if got := mainClusterForEntities(&KCLEntities{}, groups); got != "k3d-only" {
		t.Errorf("fallback should use the first k8s group's cluster, got %q", got)
	}
}
