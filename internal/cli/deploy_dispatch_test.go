package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
)

// fakeProvider is a minimal Provider used by the dispatch tests. It
// records every Deploy/Rollback invocation so tests can assert the
// dispatcher invoked the expected method on each group.
type fakeProvider struct {
	id           string
	deployCalls  []deploytarget.ServiceGroup
	rollbackArgs []rollbackCall
	deployErr    error
	rollbackErr  error
}

type rollbackCall struct {
	group       deploytarget.ServiceGroup
	lastGoodTag string
}

func (f *fakeProvider) Name() string { return f.id }

func (f *fakeProvider) Deploy(_ context.Context, g deploytarget.ServiceGroup) error {
	f.deployCalls = append(f.deployCalls, g)
	return f.deployErr
}

func (f *fakeProvider) Rollback(_ context.Context, g deploytarget.ServiceGroup, last string) error {
	f.rollbackArgs = append(f.rollbackArgs, rollbackCall{group: g, lastGoodTag: last})
	return f.rollbackErr
}

// TestRollbackDeployGroups_CallsRollback confirms the rollback
// dispatcher invokes the provider's Rollback (not Deploy) for each
// group, and only after the per-service state file is on disk.
func TestRollbackDeployGroups_CallsRollback(t *testing.T) {
	dir := t.TempDir()
	if _, err := deploytarget.WriteDeployState(dir, "external", "prod", "edge", deploytarget.DeployState{
		Image: "x/edge", Tag: "v1.0.0",
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	fp := &fakeProvider{id: "external"}
	reg := &deploytarget.Registry{}
	reg.Register(fp)
	groups := []deploytarget.ServiceGroup{
		{
			Env: "prod", ProviderID: "external",
			Services: []deploytarget.ResolvedService{
				{Name: "edge", External: &deploytarget.ExternalSpec{}},
			},
		},
	}
	if err := rollbackDeployGroups(context.Background(), reg, groups, dir); err != nil {
		t.Fatalf("rollbackDeployGroups: %v", err)
	}
	if len(fp.deployCalls) != 0 {
		t.Errorf("Rollback should not call Deploy, got %d calls", len(fp.deployCalls))
	}
	if len(fp.rollbackArgs) != 1 {
		t.Fatalf("want 1 Rollback call, got %d", len(fp.rollbackArgs))
	}
}

// TestRollbackDeployGroups_MissingStateError confirms a service
// without a recorded last-good deploy produces a clear per-service
// error and never reaches the provider.
func TestRollbackDeployGroups_MissingStateError(t *testing.T) {
	dir := t.TempDir()
	fp := &fakeProvider{id: "external"}
	reg := &deploytarget.Registry{}
	reg.Register(fp)
	groups := []deploytarget.ServiceGroup{
		{
			Env: "prod", ProviderID: "external",
			Services: []deploytarget.ResolvedService{
				{Name: "edge", External: &deploytarget.ExternalSpec{}},
			},
		},
	}
	err := rollbackDeployGroups(context.Background(), reg, groups, dir)
	if err == nil {
		t.Fatal("expected error for missing state file, got nil")
	}
	want := "no previous deploy state recorded for edge at prod; cannot rollback"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("want %q in error, got %v", want, err)
	}
	if len(fp.rollbackArgs) != 0 {
		t.Errorf("provider Rollback should not run when state missing, got %d calls", len(fp.rollbackArgs))
	}
}

// TestRollbackDeployGroups_K8sClusterSkipsStateCheck confirms the
// k8s-cluster path does NOT require a state file (kubectl rollout
// undo tracks history in-cluster).
func TestRollbackDeployGroups_K8sClusterSkipsStateCheck(t *testing.T) {
	dir := t.TempDir()
	fp := &fakeProvider{id: "k8s-cluster"}
	reg := &deploytarget.Registry{}
	reg.Register(fp)
	groups := []deploytarget.ServiceGroup{
		{
			Env: "prod", ProviderID: "k8s-cluster", Namespace: "ns-prod",
			Services: []deploytarget.ResolvedService{
				{Name: "api", K8sCluster: &deploytarget.K8sClusterSpec{}},
			},
		},
	}
	if err := rollbackDeployGroups(context.Background(), reg, groups, dir); err != nil {
		t.Fatalf("rollbackDeployGroups: %v", err)
	}
	if len(fp.rollbackArgs) != 1 {
		t.Fatalf("want 1 Rollback call, got %d", len(fp.rollbackArgs))
	}
}

// TestRollbackDeployGroups_DryRunPropagates confirms the DryRun flag
// stays on each group as the dispatcher passes it to Rollback —
// providers honor it on their end (Item 1).
func TestRollbackDeployGroups_DryRunPropagates(t *testing.T) {
	dir := t.TempDir()
	if _, err := deploytarget.WriteDeployState(dir, "external", "prod", "edge", deploytarget.DeployState{
		Image: "x/edge", Tag: "v1.0.0",
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	fp := &fakeProvider{id: "external"}
	reg := &deploytarget.Registry{}
	reg.Register(fp)
	groups := []deploytarget.ServiceGroup{
		{
			Env: "prod", ProviderID: "external", DryRun: true,
			Services: []deploytarget.ResolvedService{
				{Name: "edge", External: &deploytarget.ExternalSpec{}},
			},
		},
	}
	if err := rollbackDeployGroups(context.Background(), reg, groups, dir); err != nil {
		t.Fatalf("rollbackDeployGroups: %v", err)
	}
	if len(fp.rollbackArgs) != 1 {
		t.Fatalf("want 1 Rollback call, got %d", len(fp.rollbackArgs))
	}
	if !fp.rollbackArgs[0].group.DryRun {
		t.Errorf("dispatcher should pass DryRun=true through to Rollback")
	}
}

// TestRollbackDeployGroups_RegistryNil rejects a nil registry up
// front rather than nil-panicking inside the loop.
func TestRollbackDeployGroups_RegistryNil(t *testing.T) {
	if err := rollbackDeployGroups(context.Background(), nil, nil, ""); err == nil {
		t.Error("expected error for nil registry")
	}
}

// TestRollbackDeployGroups_PropagatesProviderError confirms a
// provider Rollback failure aborts the loop and wraps with the
// provider id.
func TestRollbackDeployGroups_PropagatesProviderError(t *testing.T) {
	dir := t.TempDir()
	if _, err := deploytarget.WriteDeployState(dir, "external", "prod", "edge", deploytarget.DeployState{
		Image: "x/edge", Tag: "v1.0.0",
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	fp := &fakeProvider{id: "external", rollbackErr: errors.New("flyctl boom")}
	reg := &deploytarget.Registry{}
	reg.Register(fp)
	groups := []deploytarget.ServiceGroup{
		{
			Env: "prod", ProviderID: "external",
			Services: []deploytarget.ResolvedService{
				{Name: "edge", External: &deploytarget.ExternalSpec{}},
			},
		},
	}
	err := rollbackDeployGroups(context.Background(), reg, groups, dir)
	if err == nil {
		t.Fatal("expected provider error, got nil")
	}
	if !strings.Contains(err.Error(), "rollback external") || !strings.Contains(err.Error(), "flyctl boom") {
		t.Errorf("error should wrap provider id + provider error, got %v", err)
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

// TestDeployCmd_RollbackFlagRegistered confirms `--rollback` is
// declared with a sensible help line.
func TestDeployCmd_RollbackFlagRegistered(t *testing.T) {
	cmd := newDeployCmd()
	f := cmd.Flags().Lookup("rollback")
	if f == nil {
		t.Fatal("--rollback flag not registered")
	}
	if !strings.Contains(f.Usage, "Roll back") {
		t.Errorf("--rollback usage should mention 'Roll back', got %q", f.Usage)
	}
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

// TestDeployCmd_RollbackAndTagMutuallyExclusive confirms a
// --rollback + --tag combination is rejected at flag-parse time. The
// rollback path reads the per-service state file for the target tag;
// accepting a caller-supplied --tag alongside would silently shadow
// the recorded value (a confusing footgun).
//
// We test by invoking the cobra command directly. The mutual-exclusion
// check fires inside RunE BEFORE runDeploy ever loads forge.yaml, so a
// chdir-to-tempdir setup isn't necessary — the check refuses fast.
func TestDeployCmd_RollbackAndTagMutuallyExclusive(t *testing.T) {
	cmd := newDeployCmd()
	cmd.SetArgs([]string{"prod", "--rollback", "--tag", "v9"})
	// Silence cobra's stderr usage printout.
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected mutual-exclusion error")
	}
	if !strings.Contains(err.Error(), "--rollback and --tag are mutually exclusive") {
		t.Errorf("error should mention mutual exclusion, got %v", err)
	}
}
