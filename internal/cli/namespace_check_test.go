package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// newCfgWithEnv builds a minimal ProjectConfig with one environment
// for namespaceResolutionSource testing. Namespace stays empty when ns
// is "" (matching the silent-failure path).
func newCfgWithEnv(envName, ns string) *config.ProjectConfig {
	return &config.ProjectConfig{
		Name: "bar",
		Envs: []config.EnvironmentConfig{
			{Name: envName, Namespace: ns},
		},
	}
}

// TestCheckNamespaceConsistency_TableDriven covers the four primary
// scenarios spelled out in the friction-log brief plus the multi-
// namespace edge case:
//
//   - "default + default match" → happy path with no explicit forge.yaml
//     declaration and a KCL render that emits no namespace literal
//     anywhere.
//   - "explicit forge.yaml matches KCL hardcode" → happy path with both
//     sides declaring the same namespace.
//   - "metadata mismatch" → KCL stamps metadata.namespace=foo but
//     forge.yaml has no declaration so the effective ns is
//     `<project>-<env>`.
//   - "env-var DNS mismatch" → KCL embeds `*.foo.svc.cluster.local`
//     in a NATS_URL env-var but no metadata.namespace; effective ns
//     differs.
//   - "both mismatches in one render" → covers the cp-forge
//     reproducer exactly.
func TestCheckNamespaceConsistency_TableDriven(t *testing.T) {
	tests := []struct {
		name              string
		manifests         string
		projectName       string
		envName           string
		effectiveNS       string
		effectiveNSSource string
		wantErr           bool
		wantErrContains   []string
	}{
		{
			name: "default namespace, KCL emits no literal namespace — passes",
			manifests: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api-server
spec:
  template:
    spec:
      containers:
        - name: api
          image: example/api:v1
          env:
            - name: LOG_LEVEL
              value: info
`,
			projectName:       "cp-forge",
			envName:           "dev",
			effectiveNS:       "cp-forge-dev",
			effectiveNSSource: "default `<project>-<env>` (no `environments[dev].namespace` in forge.yaml)",
			wantErr:           false,
		},
		{
			name: "explicit forge.yaml namespace matches KCL hardcode — passes",
			manifests: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api-server
  namespace: foo
spec:
  template:
    spec:
      containers:
        - name: api
          image: example/api:v1
          env:
            - name: NATS_URL
              value: nats://nats.foo.svc.cluster.local:4222
`,
			projectName:       "bar",
			envName:           "baz",
			effectiveNS:       "foo",
			effectiveNSSource: "explicit `environments[baz].namespace` in forge.yaml",
			wantErr:           false,
		},
		{
			name: "metadata.namespace mismatch only — fails",
			manifests: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api-server
  namespace: foo
spec:
  template:
    spec:
      containers:
        - name: api
          image: example/api:v1
`,
			projectName:       "bar",
			envName:           "baz",
			effectiveNS:       "bar-baz",
			effectiveNSSource: "default `<project>-<env>` (no `environments[baz].namespace` in forge.yaml)",
			wantErr:           true,
			wantErrContains: []string{
				`namespace mismatch`,
				`will apply to namespace: bar-baz`,
				`default ` + "`<project>-<env>`",
				`metadata.namespace`,
				`ns="foo"`,
				`Deployment/api-server`,
				`environments[baz].namespace`,
				`namespace: foo`,
				`forge deploy baz --namespace foo`,
			},
		},
		{
			name: "env-var DNS mismatch only (no metadata.namespace) — fails",
			manifests: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api-server
spec:
  template:
    spec:
      containers:
        - name: api
          image: example/api:v1
          env:
            - name: NATS_URL
              value: nats://nats.foo.svc.cluster.local:4222
            - name: TEMPORAL_HOST
              value: temporal-frontend.foo.svc.cluster.local:7233
`,
			projectName:       "bar",
			envName:           "baz",
			effectiveNS:       "bar-baz",
			effectiveNSSource: "default `<project>-<env>` (no `environments[baz].namespace` in forge.yaml)",
			wantErr:           true,
			wantErrContains: []string{
				`namespace mismatch`,
				`will apply to namespace: bar-baz`,
				`*.<ns>.svc.cluster.local`,
				`ns="foo"`,
				`NATS_URL=nats://nats.foo.svc.cluster.local:4222`,
				`TEMPORAL_HOST=temporal-frontend.foo.svc.cluster.local:7233`,
				`namespace: foo`,
			},
		},
		{
			name: "both metadata + env-var mismatch (cp-forge reproducer) — fails",
			manifests: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: workspace-controller
  namespace: cp-forge-dev
spec:
  template:
    spec:
      containers:
        - name: ctl
          image: example/ctl:v1
          env:
            - name: NATS_URL
              value: nats://nats.cp-forge-dev.svc.cluster.local:4222
            - name: LITELLM_URL
              value: http://litellm.cp-forge-dev.svc.cluster.local:4000
---
apiVersion: v1
kind: Service
metadata:
  name: workspace-controller
  namespace: cp-forge-dev
spec:
  ports: []
`,
			projectName:       "cp-forge",
			envName:           "dev-host",
			effectiveNS:       "cp-forge-dev-host",
			effectiveNSSource: "default `<project>-<env>` (no `environments[dev-host].namespace` in forge.yaml)",
			wantErr:           true,
			wantErrContains: []string{
				`namespace mismatch`,
				`will apply to namespace: cp-forge-dev-host`,
				`ns="cp-forge-dev"`,
				`Deployment/workspace-controller`,
				`Service/workspace-controller`,
				`NATS_URL=nats://nats.cp-forge-dev.svc.cluster.local:4222`,
				`environments:`,
				`namespace: cp-forge-dev`,
				`forge deploy dev-host --namespace cp-forge-dev`,
			},
		},
		{
			name:              "empty manifests — passes (no findings)",
			manifests:         "",
			projectName:       "bar",
			envName:           "baz",
			effectiveNS:       "bar-baz",
			effectiveNSSource: "default",
			wantErr:           false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkNamespaceConsistency(tc.manifests, tc.projectName, tc.envName, tc.effectiveNS, tc.effectiveNSSource)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				msg := err.Error()
				for _, want := range tc.wantErrContains {
					if !strings.Contains(msg, want) {
						t.Errorf("expected error message to contain %q\n\ngot:\n%s", want, msg)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

// TestExtractNamespacesFromValue exercises the regex that fishes
// namespaces out of in-cluster DNS values. Covers single + multi-
// occurrence inputs and verifies we don't false-positive on values
// that happen to mention `svc` or `cluster.local` in another context.
func TestExtractNamespacesFromValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"plain DNS no port", "nats.foo.svc.cluster.local", []string{"foo"}},
		{"DNS with scheme + port", "nats://nats.foo.svc.cluster.local:4222", []string{"foo"}},
		{"https url to cluster DNS", "https://api.bar-baz.svc.cluster.local/v1/health", []string{"bar-baz"}},
		{"two distinct namespaces in one value", "primary=svc.alpha.svc.cluster.local;secondary=svc.beta.svc.cluster.local", []string{"alpha", "beta"}},
		{"duplicate same namespace", "a.foo.svc.cluster.local,b.foo.svc.cluster.local", []string{"foo"}},
		{"no cluster.local suffix — not a match", "https://example.com/svc/path", nil},
		{"plain hostname", "redis-primary", nil},
		{"empty", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractNamespacesFromValue(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d]: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestResolveEffectiveNamespace covers both branches of the helper
// that mirrors runDeploy's resolution rule.
func TestResolveEffectiveNamespace(t *testing.T) {
	t.Run("explicit declaration wins", func(t *testing.T) {
		ns, src := resolveEffectiveNamespace("bar", "baz", "custom-ns")
		if ns != "custom-ns" {
			t.Errorf("got ns=%q, want custom-ns", ns)
		}
		if !strings.Contains(src, "explicit") {
			t.Errorf("expected source to mention 'explicit', got %q", src)
		}
	})

	t.Run("default <project>-<env>", func(t *testing.T) {
		ns, src := resolveEffectiveNamespace("bar", "baz", "")
		if ns != "bar-baz" {
			t.Errorf("got ns=%q, want bar-baz", ns)
		}
		if !strings.Contains(src, "default") {
			t.Errorf("expected source to mention 'default', got %q", src)
		}
	})
}

// TestPickSuggestedNamespace_MajorityWins verifies the suggested-fix
// heuristic prefers the most-referenced wrong namespace (typically
// what the user actually intended).
func TestPickSuggestedNamespace_MajorityWins(t *testing.T) {
	r := &namespaceCheckResult{
		MetadataMismatches: []metadataNamespaceMismatch{
			{Namespace: "foo", Resources: []string{"Deployment/a", "Service/a"}},
			{Namespace: "bar", Resources: []string{"Deployment/b"}},
		},
		EnvVarMismatches: []envVarNamespaceMismatch{
			{Namespace: "foo", Occurrences: []envVarOccurrence{{Owner: "Deployment/a", EnvName: "X", EnvValue: "x"}}},
		},
	}
	if got := pickSuggestedNamespace(r); got != "foo" {
		t.Errorf("majority winner: got %q, want foo", got)
	}
}

// TestNamespaceResolutionSource exercises the runDeploy-side helper
// that picks the source description based on the deployOptions flags
// and forge.yaml. Confirms each branch surfaces the right text so the
// fix hint matches the actual silent-failure mode.
func TestNamespaceResolutionSource(t *testing.T) {
	// Re-import the config package locally for the helper signature.
	// This avoids growing the test file with unused imports; the symbol
	// is referenced via cfg below.
	cases := []struct {
		name        string
		envName     string
		envInCfg    string // namespace value in cfg.Envs[].Namespace ("" = not declared)
		flagNS      string // value of --namespace flag
		wantContain string
	}{
		{"flag wins over forge.yaml", "dev", "foo", "cli-ns", "explicit `--namespace cli-ns`"},
		{"forge.yaml declared", "dev", "foo", "", "explicit `environments[dev].namespace`"},
		{"default path (silent-failure)", "dev", "", "", "default `<project>-<env>`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newCfgWithEnv(tc.envName, tc.envInCfg)
			got := namespaceResolutionSource(cfg, tc.envName, tc.flagNS)
			if !strings.Contains(got, tc.wantContain) {
				t.Errorf("got %q, want substring %q", got, tc.wantContain)
			}
		})
	}
}
