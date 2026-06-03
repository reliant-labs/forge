package cli

import (
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestExpectedClusterForEnv_DevDefault confirms the dev default
// of `k3d-<project>` so existing projects don't need to add a
// `cluster:` field to forge.yaml to benefit from the guard.
func TestExpectedClusterForEnv_DevDefault(t *testing.T) {
	cfg := &config.ProjectConfig{Name: "cp-forge"}
	got := expectedClusterForEnv(cfg, "dev")
	want := "k3d-cp-forge"
	if got != want {
		t.Errorf("dev default: want %q, got %q", want, got)
	}
}

// TestExpectedClusterForEnv_ExplicitDeclaration confirms that
// `environments[].cluster` in forge.yaml takes precedence over
// the dev default.
func TestExpectedClusterForEnv_ExplicitDeclaration(t *testing.T) {
	cfg := &config.ProjectConfig{
		Name: "cp-forge",
		Envs: []config.EnvironmentConfig{
			{Name: "prod", Cluster: "gke_acme-prod_us-central1_cluster-1"},
		},
	}
	got := expectedClusterForEnv(cfg, "prod")
	want := "gke_acme-prod_us-central1_cluster-1"
	if got != want {
		t.Errorf("explicit declaration: want %q, got %q", want, got)
	}
}

// TestExpectedClusterForEnv_NoDeclaration returns empty for non-dev
// envs without an explicit cluster — the guard is skipped (with a
// notice), preserving backwards compatibility.
func TestExpectedClusterForEnv_NoDeclaration(t *testing.T) {
	cfg := &config.ProjectConfig{
		Name: "cp-forge",
		Envs: []config.EnvironmentConfig{
			{Name: "staging"},
		},
	}
	got := expectedClusterForEnv(cfg, "staging")
	if got != "" {
		t.Errorf("no declaration should return empty, got %q", got)
	}
}

// TestExpectedClusterForEnv_DevExplicitOverride confirms that even for
// dev, an explicit declaration wins over the k3d-<project> default —
// supports projects with a non-default k3d cluster name.
func TestExpectedClusterForEnv_DevExplicitOverride(t *testing.T) {
	cfg := &config.ProjectConfig{
		Name: "cp-forge",
		Envs: []config.EnvironmentConfig{
			{Name: "dev", Cluster: "k3d-my-custom-name"},
		},
	}
	got := expectedClusterForEnv(cfg, "dev")
	want := "k3d-my-custom-name"
	if got != want {
		t.Errorf("dev explicit override: want %q, got %q", want, got)
	}
}
