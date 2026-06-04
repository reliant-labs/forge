package cli

import (
	"runtime"
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

// TestDeployTargetArchFlagRegistered confirms `forge deploy
// --target-arch` is wired so users can cross-compile from a Mac/arm64
// host onto an amd64 cluster without editing forge.yaml.
func TestDeployTargetArchFlagRegistered(t *testing.T) {
	cmd := newDeployCmd()
	f := cmd.Flags().Lookup("target-arch")
	if f == nil {
		t.Fatal("--target-arch flag not registered on deploy command")
	}
	if f.DefValue != "" {
		t.Errorf("--target-arch default = %q, want empty", f.DefValue)
	}
}

// TestResolveDeployArch verifies the deploy-side arch resolver. Unlike
// resolveBuildArch in build.go, this one ALWAYS falls back to amd64 (no
// "host arch" branch) since deploy is always building an image for a
// cluster node. Returns the empty string only when the resolved target
// matches the host arch — the signal to skip `--platform`.
func TestResolveDeployArch(t *testing.T) {
	otherArch := "amd64"
	if runtime.GOARCH == "amd64" {
		otherArch = "arm64"
	}

	cases := []struct {
		name     string
		cfgArch  string
		flagArch string
		want     string
	}{
		{
			name:     "no config, no flag → amd64 default",
			cfgArch:  "",
			flagArch: "",
			want: func() string {
				if runtime.GOARCH == "amd64" {
					return ""
				}
				return "amd64"
			}(),
		},
		{
			name:     "cfg arch matches host → empty",
			cfgArch:  runtime.GOARCH,
			flagArch: "",
			want:     "",
		},
		{
			name:     "cfg arch differs from host → cross-compile",
			cfgArch:  otherArch,
			flagArch: "",
			want:     otherArch,
		},
		{
			name:     "flag overrides cfg",
			cfgArch:  runtime.GOARCH,
			flagArch: otherArch,
			want:     otherArch,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveDeployArch(c.cfgArch, c.flagArch)
			if got != c.want {
				t.Errorf("resolveDeployArch(cfg=%q, flag=%q) = %q, want %q",
					c.cfgArch, c.flagArch, got, c.want)
			}
		})
	}
}

// TestEffectiveTargetArch covers the DeployConfig.EffectiveTargetArch
// precedence chain: explicit override > forge.yaml field > "amd64"
// default. This is the project-level reader; the CLI-level resolver
// (resolveDeployArch) layers runtime.GOARCH comparison on top of this.
func TestEffectiveTargetArch(t *testing.T) {
	cases := []struct {
		name     string
		field    string
		override string
		want     string
	}{
		{"empty → amd64", "", "", "amd64"},
		{"field wins over default", "arm64", "", "arm64"},
		{"override wins over field", "arm64", "amd64", "amd64"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &config.DeployConfig{TargetArch: c.field}
			if got := d.EffectiveTargetArch(c.override); got != c.want {
				t.Errorf("EffectiveTargetArch(override=%q) with field=%q = %q, want %q",
					c.override, c.field, got, c.want)
			}
		})
	}
}

// Note: TestHostDeploymentSkipSet_DevOnly was removed when
// services[].dev_target moved to the KCL layer in feat/kcl-orchestration.
// The replacement filter reads `deploy: "host"` from rendered KCL — see
// deploy_kcl_test.go for the KCL-side equivalent coverage.

// TestRenderedDeploymentNames verifies the extractor parses the multi-
// document YAML stream forge produces from KCL, returning only
// Deployment kind names. Non-Deployments and malformed docs are skipped.
func TestRenderedDeploymentNames(t *testing.T) {
	manifests := `apiVersion: v1
kind: Service
metadata:
  name: workspace-controller
spec:
  ports: []
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: workspace-controller
  labels:
    app.kubernetes.io/managed-by: forge
spec: {}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cp-forge-config
data:
  KEY: value
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: workspace-proxy
spec: {}
`
	got := renderedDeploymentNames(manifests)
	want := []string{"workspace-controller", "workspace-proxy"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestRenderedDeploymentNames_EmptyAndMalformed confirms the extractor
// degrades gracefully on edge cases: empty input, all-non-Deployment,
// and unparseable docs all return an empty slice rather than panicking.
func TestRenderedDeploymentNames_EmptyAndMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace", "   \n\n  "},
		{"no Deployments", "kind: Service\nmetadata:\n  name: x\n"},
		{"malformed YAML", "this is not yaml: : :"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderedDeploymentNames(c.in); len(got) != 0 {
				t.Errorf("expected empty slice, got %v", got)
			}
		})
	}
}
