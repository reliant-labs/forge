package cli

import (
	"strings"
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

// TestKubectlContextGuardVerdict_Match returns nil when the current
// kubectl context matches the env's expected cluster — the happy path
// that lets a deploy (or dry-run) proceed.
func TestKubectlContextGuardVerdict_Match(t *testing.T) {
	if err := kubectlContextGuardVerdict("prod", "gke_acme-prod", "gke_acme-prod"); err != nil {
		t.Errorf("expected nil for matching contexts, got %v", err)
	}
}

// TestKubectlContextGuardVerdict_Mismatch returns an error when current
// differs from expected. This is the path that --dry-run now exercises
// too: dry-run is for surfacing the mistake, not papering over it.
func TestKubectlContextGuardVerdict_Mismatch(t *testing.T) {
	err := kubectlContextGuardVerdict("prod", "gke_acme-prod", "k3d-cp-forge")
	if err == nil {
		t.Fatal("expected error for mismatched contexts, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"prod", "gke_acme-prod", "k3d-cp-forge", "refusing to deploy"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got:\n%s", want, msg)
		}
	}
}

// TestKubectlContextGuardVerdict_NoExpectation returns nil when no
// expected cluster is declared — preserves backwards compat for
// projects that haven't yet added environments[].cluster.
func TestKubectlContextGuardVerdict_NoExpectation(t *testing.T) {
	if err := kubectlContextGuardVerdict("staging", "", "k3d-cp-forge"); err != nil {
		t.Errorf("expected nil when no expectation, got %v", err)
	}
}

// TestDeployDryRunHelpMentionsGuard documents that dry-run still runs
// the env-cluster guard — the change in this commit. A user reading
// `forge deploy --help` should see that.
func TestDeployDryRunHelpMentionsGuard(t *testing.T) {
	cmd := newDeployCmd()
	f := cmd.Flags().Lookup("dry-run")
	if f == nil {
		t.Fatal("--dry-run flag not registered")
	}
	if !strings.Contains(f.Usage, "guard") {
		t.Errorf("--dry-run usage should mention the env-cluster guard, got %q", f.Usage)
	}
}
