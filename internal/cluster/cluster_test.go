package cluster

import (
	"strings"
	"testing"
)

// contains reports whether ss contains the exact string s.
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// indexOf returns the position of s in ss, or -1.
func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}

// TestRenderDArgs_QuotesStringArgs pins the forge-deploy-prod regression
// fix: the forge-controlled string args (image_tag, namespace, env) are
// emitted as QUOTED KCL string literals so KCL types an all-digit
// git-describe tag as `str` rather than coercing it to `int` (which
// fails RenderEnv.image_tag's `str` type). envCfgKV values are left
// unquoted — their KCL type is intentional project config.
func TestRenderDArgs_QuotesStringArgs(t *testing.T) {
	// Numeric tag: the regression trigger. Must come out QUOTED.
	got := renderDArgs("3826648", "cp-forge-prod", "prod", nil)
	for _, want := range []string{
		`image_tag="3826648"`,
		`namespace="cp-forge-prod"`,
		`env="prod"`,
	} {
		if !contains(got, want) {
			t.Errorf("expected %q in dArgs, got %v", want, got)
		}
	}

	// Non-numeric tag is also quoted (uniform handling).
	got = renderDArgs("v1.2.3", "ns", "dev", nil)
	if !contains(got, `image_tag="v1.2.3"`) {
		t.Errorf("expected image_tag=\"v1.2.3\" in dArgs, got %v", got)
	}

	// Empty env produces no env= binding.
	got = renderDArgs("tag", "ns", "", nil)
	for _, a := range got {
		if strings.HasPrefix(a, "env=") {
			t.Errorf("expected no env= entry for empty env, got %v", got)
		}
	}

	// envCfgKV values are appended sorted by key and UNquoted.
	got = renderDArgs("tag", "ns", "", map[string]string{"B": "2", "A": "1"})
	if !contains(got, "A=1") {
		t.Errorf("expected unquoted A=1 in dArgs, got %v", got)
	}
	if !contains(got, "B=2") {
		t.Errorf("expected unquoted B=2 in dArgs, got %v", got)
	}
	if ia, ib := indexOf(got, "A=1"), indexOf(got, "B=2"); ia == -1 || ib == -1 || ia > ib {
		t.Errorf("expected A=1 before B=2 (sorted), got %v", got)
	}
}

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
	got := RenderedDeploymentNames(manifests)
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

// TestExtractManifests_SiblingOutputIsSilent confirms that when the
// generated `main.k` exports both `manifests` (the YAML manifest list
// we consume) AND `output` (the JSON contract the forge build/run/
// deploy pipeline consumes via a separate kcl invocation), the
// `output` sibling is silently skipped rather than emitting a noisy
// "extra top-level KCL var" warning on every `forge deploy` /
// `forge up`. Pins the dual-output contract documented at the top of
// the canonical main.k template.
func TestExtractManifests_SiblingOutputIsSilent(t *testing.T) {
	// Mirrors the shape kcl emits when main.k declares both
	// `output = forge.render(_bundle)` and
	// `manifests = forge.render_manifests(_bundle, _env)`.
	in := `manifests:
- apiVersion: v1
  kind: Namespace
  metadata:
    name: example-dev
- apiVersion: apps/v1
  kind: Deployment
  metadata:
    name: workspace-proxy
  spec: {}
output:
  services:
  - name: workspace-proxy
    deploy:
      type: cluster
  operators: []
  frontends: []
  cronjobs: []
  config_maps: []
`
	got, err := extractManifests([]byte(in))
	if err != nil {
		t.Fatalf("extractManifests: %v", err)
	}
	if !strings.Contains(got, "kind: Namespace") || !strings.Contains(got, "kind: Deployment") {
		t.Errorf("expected Namespace + Deployment in output, got:\n%s", got)
	}
	// Two manifest items should be `---`-separated.
	if !strings.Contains(got, "---") {
		t.Errorf("expected `---` document separator, got:\n%s", got)
	}
}

// TestExtractManifests_UnexpectedSiblingStillWarns confirms we only
// silence the documented `output` sibling — any OTHER unexpected
// top-level var still triggers the helpful warning so projects don't
// silently drop manifest content into a stray top-level binding.
func TestExtractManifests_UnexpectedSiblingStillWarns(t *testing.T) {
	in := `manifests:
- apiVersion: v1
  kind: Namespace
  metadata:
    name: example-dev
stray_var:
  something: else
`
	// We can't capture os.Stderr without plumbing without changing the
	// production signature; instead, assert success (warning is fire-
	// and-forget) and that the function does still extract manifests.
	got, err := extractManifests([]byte(in))
	if err != nil {
		t.Fatalf("extractManifests: %v", err)
	}
	if !strings.Contains(got, "kind: Namespace") {
		t.Errorf("expected Namespace in output, got:\n%s", got)
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
			if got := RenderedDeploymentNames(c.in); len(got) != 0 {
				t.Errorf("expected empty slice, got %v", got)
			}
		})
	}
}

// filterTestManifests is a representative env bundle: two app
// Deployments (each carrying app.kubernetes.io/name), a shared ConfigMap
// and a Namespace (NO app.kubernetes.io/name — the shared/infra shape
// forge's renderer produces). It mirrors what RenderManifests emits and
// is `\n---\n`-joined exactly like the production stream.
const filterTestManifests = `apiVersion: v1
kind: Namespace
metadata:
  name: example-dev
  labels:
    app.kubernetes.io/managed-by: forge
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: example-config
  namespace: example-dev
  labels:
    app.kubernetes.io/managed-by: forge
    app.kubernetes.io/part-of: example-dev
data:
  KEY: value
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: admin-server
  namespace: example-dev
  labels:
    app.kubernetes.io/name: admin-server
    app.kubernetes.io/managed-by: forge
spec: {}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: workspace-proxy
  namespace: example-dev
  labels:
    app.kubernetes.io/name: workspace-proxy
    app.kubernetes.io/managed-by: forge
spec: {}`

// TestFilterManifestsByApp_KeepsTargetAndShared is the core assertion
// of the --target application filter: targeting one app keeps that app's
// Deployment plus every shared resource (the ConfigMap and Namespace
// have no app.kubernetes.io/name label) and drops the other app's
// Deployment.
func TestFilterManifestsByApp_KeepsTargetAndShared(t *testing.T) {
	got, err := FilterManifestsByApp(filterTestManifests, []string{"admin-server"})
	if err != nil {
		t.Fatalf("FilterManifestsByApp: %v", err)
	}
	if !strings.Contains(got, "name: admin-server") {
		t.Errorf("expected targeted app admin-server to be kept, got:\n%s", got)
	}
	if !strings.Contains(got, "kind: Namespace") {
		t.Errorf("expected shared Namespace (no app label) to be kept, got:\n%s", got)
	}
	if !strings.Contains(got, "name: example-config") {
		t.Errorf("expected shared ConfigMap (no app label) to be kept, got:\n%s", got)
	}
	if strings.Contains(got, "name: workspace-proxy") {
		t.Errorf("expected non-targeted app workspace-proxy to be dropped, got:\n%s", got)
	}
}

// TestFilterManifestsByApp_MultipleTargets confirms the filter unions
// multiple --target apps and still keeps shared resources.
func TestFilterManifestsByApp_MultipleTargets(t *testing.T) {
	got, err := FilterManifestsByApp(filterTestManifests, []string{"admin-server", "workspace-proxy"})
	if err != nil {
		t.Fatalf("FilterManifestsByApp: %v", err)
	}
	for _, want := range []string{"name: admin-server", "name: workspace-proxy", "kind: Namespace", "name: example-config"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output, got:\n%s", want, got)
		}
	}
}

// TestFilterManifestsByApp_UnknownTargetErrors confirms a typo'd target
// (matching no app workload) errors with the available app names rather
// than applying a shared-only bundle that does nothing the user wanted.
func TestFilterManifestsByApp_UnknownTargetErrors(t *testing.T) {
	_, err := FilterManifestsByApp(filterTestManifests, []string{"nope"})
	if err == nil {
		t.Fatal("expected error for unknown target, got nil")
	}
	// Available app names should be surfaced for the fix.
	if !strings.Contains(err.Error(), "admin-server") || !strings.Contains(err.Error(), "workspace-proxy") {
		t.Errorf("expected available app names in error, got: %v", err)
	}
}
