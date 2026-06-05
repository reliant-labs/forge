package cli

import (
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestKCLBuildPlanHelpers covers the small helpers that runBuild uses
// to drive the docker-skip set and the platform override from a parsed
// KCL entity set. The runBuild path itself is exercised end-to-end by
// the cp-forge smoke (post-agent-A) — these unit tests guard the
// dispatch / accessor invariants.
func TestKCLBuildPlanHelpers(t *testing.T) {
	entities, err := parseKCLEntities([]byte(sampleKCLJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}

	if !kclHasClusterService(entities) {
		t.Error("kclHasClusterService: want true (sample has workspace-proxy)")
	}
	if got := kclFirstClusterPlatform(entities); got != "amd64" {
		t.Errorf("kclFirstClusterPlatform: got %q, want amd64", got)
	}
}

// TestKCLHasClusterService_AllHost confirms the all-host-services
// scenario flips the docker-skip switch — runBuild uses this to decide
// whether the project docker image is needed at all for the env.
func TestKCLHasClusterService_AllHost(t *testing.T) {
	allHost := `{
  "services": [
    {"name": "a", "deploy": {"type": "host", "runner": "go-run"}},
    {"name": "b", "deploy": {"type": "build-only", "build_variants": [{"name": "default"}]}}
  ]
}`
	entities, err := parseKCLEntities([]byte(allHost))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if kclHasClusterService(entities) {
		t.Error("kclHasClusterService: want false when no cluster service")
	}
	if got := kclFirstClusterPlatform(entities); got != "" {
		t.Errorf("kclFirstClusterPlatform on no-cluster: got %q, want empty", got)
	}
}

// TestFilterFrontendsForBuild pins Item 3: host-mode frontends are
// dropped from the prod-build set (their dev server doesn't consume
// the build artifact); cluster-mode frontends are kept; and frontends
// with no KCL deploy block (legacy) fall through to "build" so we
// don't silently change behaviour for projects pre-deploy-discriminator.
func TestFilterFrontendsForBuild(t *testing.T) {
	frontends := []config.FrontendConfig{
		{Name: "web", Path: "frontend"},
		{Name: "admin", Path: "admin"},
		{Name: "legacy", Path: "legacy"},
	}
	entities := &KCLEntities{
		Frontends: []FrontendEntity{
			{Name: "web", Deploy: &FrontendDeployEntity{Type: "host"}},
			{Name: "admin", Deploy: &FrontendDeployEntity{Type: "cluster"}},
			// "legacy" has no Deploy block — falls through to "build".
			{Name: "legacy"},
		},
	}
	got := filterFrontendsForBuild(frontends, entities)
	if len(got) != 2 {
		t.Fatalf("filterFrontendsForBuild: got %d kept, want 2 (admin + legacy)", len(got))
	}
	names := map[string]bool{}
	for _, fe := range got {
		names[fe.Name] = true
	}
	if names["web"] {
		t.Errorf("filterFrontendsForBuild: host-mode 'web' was kept; want skipped")
	}
	if !names["admin"] {
		t.Errorf("filterFrontendsForBuild: cluster-mode 'admin' was dropped; want kept")
	}
	if !names["legacy"] {
		t.Errorf("filterFrontendsForBuild: legacy 'legacy' (no deploy) was dropped; want kept")
	}
}

// TestFrontendDeployMode covers the lookup helper across its three
// branches: matching frontend with deploy → type; matching frontend
// without deploy → ""; missing frontend → "".
func TestFrontendDeployMode(t *testing.T) {
	entities := &KCLEntities{
		Frontends: []FrontendEntity{
			{Name: "web", Deploy: &FrontendDeployEntity{Type: "Host"}},
			{Name: "admin", Deploy: &FrontendDeployEntity{Type: "cluster"}},
			{Name: "legacy"},
		},
	}
	if got := frontendDeployMode(entities, "web"); got != "host" {
		t.Errorf("web: got %q, want host (case-folded)", got)
	}
	if got := frontendDeployMode(entities, "admin"); got != "cluster" {
		t.Errorf("admin: got %q, want cluster", got)
	}
	if got := frontendDeployMode(entities, "legacy"); got != "" {
		t.Errorf("legacy (no deploy): got %q, want empty", got)
	}
	if got := frontendDeployMode(entities, "missing"); got != "" {
		t.Errorf("missing frontend: got %q, want empty", got)
	}
	if got := frontendDeployMode(nil, "web"); got != "" {
		t.Errorf("nil entities: got %q, want empty", got)
	}
}
