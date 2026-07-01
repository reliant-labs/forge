package cli

import (
	"strings"
	"testing"
)

// inTargetSet membership semantics (empty filter matches everything;
// non-empty is an exact-name allowlist) are covered by up_test.go's
// TestInTargetSet — the helper is shared with `forge up`'s --target.

// TestFilterEntitiesByTarget confirms the entity-layer filter narrows
// Services, Operators, and Frontends to the targeted names while
// carrying every other entity slice through unchanged.
func TestFilterEntitiesByTarget(t *testing.T) {
	e := &KCLEntities{
		Services: []ServiceEntity{
			{Name: "admin-server"},
			{Name: "workspace-proxy"},
		},
		Operators: []OperatorEntity{
			{Name: "workspace-controller"},
			{Name: "billing-controller"},
		},
		Frontends: []FrontendEntity{
			{Name: "admin-ui"},
			{Name: "marketing"},
		},
		CronJobs: []CronJobEntity{{Name: "migrate"}},
	}
	got := filterEntitiesByTarget(e, []string{"admin-server", "admin-ui"})

	if len(got.Services) != 1 || got.Services[0].Name != "admin-server" {
		t.Errorf("services: got %+v, want [admin-server]", got.Services)
	}
	if len(got.Frontends) != 1 || got.Frontends[0].Name != "admin-ui" {
		t.Errorf("frontends: got %+v, want [admin-ui]", got.Frontends)
	}
	// No operator was targeted → operator slice empties out (so the
	// frontendOnly gate and the operator-derived sets don't see a
	// non-targeted operator).
	if len(got.Operators) != 0 {
		t.Errorf("operators: got %+v, want []", got.Operators)
	}
	// Non-app-name entities are carried through untouched.
	if len(got.CronJobs) != 1 || got.CronJobs[0].Name != "migrate" {
		t.Errorf("cronjobs should be carried through unchanged, got %+v", got.CronJobs)
	}
	// The original must not be mutated (shallow copy of the struct).
	if len(e.Services) != 2 || len(e.Operators) != 2 {
		t.Errorf("input entities mutated: svcs=%+v ops=%+v", e.Services, e.Operators)
	}
}

// TestFilterEntitiesByTarget_Operator confirms that naming an operator
// keeps just that operator (and drops the services / frontends / other
// operators), making `forge deploy <env> --target <operator>` deploy
// only the operator's workload. The K8sCluster manifest filter does the
// load-bearing scoping via the app label; this entity-layer narrowing is
// what keeps the operator-derived sets consistent.
func TestFilterEntitiesByTarget_Operator(t *testing.T) {
	e := &KCLEntities{
		Services:  []ServiceEntity{{Name: "admin-server"}},
		Operators: []OperatorEntity{{Name: "workspace-controller"}, {Name: "billing-controller"}},
		Frontends: []FrontendEntity{{Name: "admin-ui"}},
	}
	got := filterEntitiesByTarget(e, []string{"workspace-controller"})

	if len(got.Operators) != 1 || got.Operators[0].Name != "workspace-controller" {
		t.Errorf("operators: got %+v, want [workspace-controller]", got.Operators)
	}
	if len(got.Services) != 0 {
		t.Errorf("services should be empty when only an operator is targeted, got %+v", got.Services)
	}
	if len(got.Frontends) != 0 {
		t.Errorf("frontends should be empty when only an operator is targeted, got %+v", got.Frontends)
	}
}

// TestFilterEntitiesToFrontendsOnly confirms the --frontends-only narrowing
// drops EVERY non-frontend kind — most importantly CronJobs, which
// filterEntitiesByTarget deliberately carries through. A project declaring a
// forge.CronJob (e.g. a one-shot schema-migration Job) would otherwise keep
// entities.CronJobs non-empty, leave the frontendOnly cluster-skip guard
// false, and drive an empty-manifest cluster.Apply that dies "no objects
// passed to apply". After this filter the only surviving kind is Frontends,
// which is exactly what makes that guard engage.
func TestFilterEntitiesToFrontendsOnly(t *testing.T) {
	e := &KCLEntities{
		Services:  []ServiceEntity{{Name: "admin-server", Deploy: DeployConfigEntity{Type: "cluster"}}},
		Operators: []OperatorEntity{{Name: "workspace-controller"}},
		Frontends: []FrontendEntity{
			{Name: "reliant-web", Deploy: &FrontendDeployEntity{Type: "firebase"}},
			{Name: "admin-web"}, // deploy = None build-only; must survive
		},
		CronJobs:   []CronJobEntity{{Name: "control-plane-migrate", Schedule: ""}},
		Gateways:   []GatewayEntity{{Name: "gw"}},
		HelmCharts: []HelmChartEntity{{Name: "cert-manager"}},
	}

	got := filterEntitiesToFrontendsOnly(e)

	// Every frontend survives — the Firebase one AND the build-only one it
	// bundles.
	if len(got.Frontends) != 2 {
		t.Errorf("frontends should be carried through unchanged, got %+v", got.Frontends)
	}
	// Every other kind is dropped.
	if len(got.Services) != 0 || len(got.Operators) != 0 || len(got.CronJobs) != 0 ||
		len(got.Gateways) != 0 || len(got.HelmCharts) != 0 {
		t.Errorf("non-frontend kinds should be stripped, got svcs=%+v ops=%+v cron=%+v gw=%+v helm=%+v",
			got.Services, got.Operators, got.CronJobs, got.Gateways, got.HelmCharts)
	}

	// The exact predicate runDeploy uses to skip the cluster pipeline. With
	// the CronJob (and everything else) stripped, it now evaluates true —
	// the regression this fix closes.
	frontendOnly := !kclEntitiesHaveK8sCluster(got) && hasFirebaseFrontend(got) &&
		len(got.Operators) == 0 && len(got.CronJobs) == 0
	if !frontendOnly {
		t.Errorf("frontendOnly guard should engage after the filter; got false")
	}

	// The original must not be mutated (shallow copy of the struct).
	if len(e.CronJobs) != 1 || len(e.Services) != 1 {
		t.Errorf("input entities mutated: cron=%+v svcs=%+v", e.CronJobs, e.Services)
	}
}

// TestValidateDeployTargets_Unknown confirms a typo'd target errors with
// the list of available app names (services + operators + frontends),
// and that a fully-valid target set passes.
func TestValidateDeployTargets_Unknown(t *testing.T) {
	e := &KCLEntities{
		Services:  []ServiceEntity{{Name: "admin-server"}},
		Operators: []OperatorEntity{{Name: "workspace-controller"}},
		Frontends: []FrontendEntity{{Name: "admin-ui"}},
	}

	if err := validateDeployTargets(e, []string{"admin-server", "admin-ui"}); err != nil {
		t.Fatalf("valid targets should pass, got: %v", err)
	}

	err := validateDeployTargets(e, []string{"nope"})
	if err == nil {
		t.Fatal("expected error for unknown target, got nil")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the bad target, got: %v", err)
	}
	if !strings.Contains(err.Error(), "admin-server") || !strings.Contains(err.Error(), "admin-ui") {
		t.Errorf("error should list available apps, got: %v", err)
	}
	// The operator must appear in the available-apps list, too.
	if !strings.Contains(err.Error(), "workspace-controller") {
		t.Errorf("error should list operators as available apps, got: %v", err)
	}
}

// TestValidateDeployTargets_Operator confirms an operator name is a valid
// --target subject — the regression this change fixes (operators were
// previously absent from the available-apps set, so targeting one errored
// "unknown --target").
func TestValidateDeployTargets_Operator(t *testing.T) {
	e := &KCLEntities{
		Services:  []ServiceEntity{{Name: "admin-server"}},
		Operators: []OperatorEntity{{Name: "workspace-controller"}},
	}
	if err := validateDeployTargets(e, []string{"workspace-controller"}); err != nil {
		t.Fatalf("operator target should be accepted, got: %v", err)
	}
}
