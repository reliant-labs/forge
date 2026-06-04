package cli

import (
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestHostDeploymentSkipSetFromKCL verifies the rollout-skip set built
// from a rendered KCL entity contains both the bare and project-prefixed
// Deployment names for every host-mode service. Per-service-binary mode
// renders `<svc>`; shared-binary mode renders `<project>-<svc>` — the
// rollout-wait loop iterates over both shapes, so we pre-expand to keep
// the caller's hot path branch-free.
func TestHostDeploymentSkipSetFromKCL(t *testing.T) {
	cfg := &config.ProjectConfig{Name: "cp-forge"}
	entities, err := parseKCLEntities([]byte(sampleKCLJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}

	got := hostDeploymentSkipSetFromKCL(cfg, entities)
	want := []string{"admin-server", "cp-forge-admin-server"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d (%v)", len(got), len(want), got)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %q in skip set %v", k, got)
		}
	}
}

func TestHostDeploymentSkipSetFromKCL_NilInputs(t *testing.T) {
	if got := hostDeploymentSkipSetFromKCL(nil, nil); len(got) != 0 {
		t.Errorf("nil cfg+entities: got %v, want empty", got)
	}
	if got := hostDeploymentSkipSetFromKCL(&config.ProjectConfig{Name: "x"}, nil); len(got) != 0 {
		t.Errorf("nil entities: got %v, want empty", got)
	}
}
