package cli

import (
	"strings"
	"testing"
)

// inTargetSet membership semantics (empty filter matches everything;
// non-empty is an exact-name allowlist) are covered by up_test.go's
// TestInTargetSet — the helper is shared with `forge up`'s --target.

// TestFilterEntitiesByTarget confirms the entity-layer filter narrows
// Services and Frontends to the targeted names while carrying every
// other entity slice through unchanged.
func TestFilterEntitiesByTarget(t *testing.T) {
	e := &KCLEntities{
		Services: []ServiceEntity{
			{Name: "admin-server"},
			{Name: "workspace-proxy"},
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
	// Non-app-name entities are carried through untouched.
	if len(got.CronJobs) != 1 || got.CronJobs[0].Name != "migrate" {
		t.Errorf("cronjobs should be carried through unchanged, got %+v", got.CronJobs)
	}
	// The original must not be mutated (shallow copy of the struct).
	if len(e.Services) != 2 {
		t.Errorf("input entities mutated: %+v", e.Services)
	}
}

// TestValidateDeployTargets_Unknown confirms a typo'd target errors with
// the list of available app names (services + frontends), and that a
// fully-valid target set passes.
func TestValidateDeployTargets_Unknown(t *testing.T) {
	e := &KCLEntities{
		Services:  []ServiceEntity{{Name: "admin-server"}},
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
}
