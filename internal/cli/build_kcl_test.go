package cli

import "testing"

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
