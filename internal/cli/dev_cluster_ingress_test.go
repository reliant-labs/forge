package cli

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/templates"
)

// TestSpliceK3dPorts_AppendsToExisting covers the canonical case —
// scaffolded deploy/k3d.yaml has its own ports[] block and the
// generated fragment carries one or more listener mappings. Output
// must contain BOTH the user entries and the fragment entries.
func TestSpliceK3dPorts_AppendsToExisting(t *testing.T) {
	user := []byte(`apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: example
ports:
  - port: 18080:80
    nodeFilters:
      - loadbalancer
`)
	fragment := []byte(`ports:
  - port: 19190:19190
    nodeFilters:
      - loadbalancer
`)
	out, err := spliceK3dPorts(user, fragment)
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parse merged: %v\n%s", err, out)
	}
	ports, ok := parsed["ports"].([]any)
	if !ok {
		t.Fatalf("merged ports[] not a list: %T\n%s", parsed["ports"], out)
	}
	if len(ports) != 2 {
		t.Errorf("merged ports[] count = %d, want 2\n%s", len(ports), out)
	}
	// Metadata + apiVersion pass through.
	if !strings.Contains(string(out), "name: example") {
		t.Errorf("merged YAML lost metadata.name:\n%s", out)
	}
}

// TestSpliceK3dPorts_NoUserPortsBlock covers the case where the user
// removed the ports[] block from deploy/k3d.yaml (relying entirely
// on the generated fragment). Output must contain just the fragment
// entries.
func TestSpliceK3dPorts_NoUserPortsBlock(t *testing.T) {
	user := []byte(`apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: noports
`)
	fragment := []byte(`ports:
  - port: 18080:18080
    nodeFilters:
      - loadbalancer
  - port: 19190:19190
    nodeFilters:
      - loadbalancer
`)
	out, err := spliceK3dPorts(user, fragment)
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parse merged: %v\n%s", err, out)
	}
	ports := parsed["ports"].([]any)
	if len(ports) != 2 {
		t.Errorf("merged ports[] count = %d, want 2\n%s", len(ports), out)
	}
}

// TestSpliceK3dPorts_EmptyFragmentNoOp covers the case where the
// fragment has no ports entries (e.g. the dev env has no gateways).
// Output should be the user YAML verbatim — preserve user comments
// and formatting by skipping the marshal round-trip.
func TestSpliceK3dPorts_EmptyFragmentNoOp(t *testing.T) {
	user := []byte(`# user comment preserved
apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: empty-frag
ports:
  - port: 18080:80
    nodeFilters: [loadbalancer]
`)
	fragment := []byte(`# fragment with no ports
`)
	out, err := spliceK3dPorts(user, fragment)
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	if string(out) != string(user) {
		t.Errorf("expected pass-through; got divergent output\n--- got ---\n%s\n--- want ---\n%s", out, user)
	}
}

// TestSpliceK3dPorts_FragmentWinsOnHostCollision pins the dedupe
// policy: when the scaffolded YAML and the fragment both claim host
// port 18080 with different cluster-side targets, the fragment wins.
// This is the canonical bug — scaffolded `18080:80` (pre-Gateway-API
// artifact) collided with fragment `18080:18080` (from the new KCL
// listener), and k3d refused the config.
func TestSpliceK3dPorts_FragmentWinsOnHostCollision(t *testing.T) {
	user := []byte(`apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: collision
ports:
  - port: 18080:80
    nodeFilters:
      - loadbalancer
`)
	fragment := []byte(`ports:
  - port: 18080:18080
    nodeFilters:
      - loadbalancer
`)
	out, err := spliceK3dPorts(user, fragment)
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parse merged: %v\n%s", err, out)
	}
	ports, ok := parsed["ports"].([]any)
	if !ok {
		t.Fatalf("merged ports[] not a list: %T\n%s", parsed["ports"], out)
	}
	if len(ports) != 1 {
		t.Fatalf("merged ports[] count = %d, want 1 (dedupe should drop scaffold entry)\n%s", len(ports), out)
	}
	entry, ok := ports[0].(map[string]any)
	if !ok {
		t.Fatalf("ports[0] not a map: %T", ports[0])
	}
	if got, want := entry["port"], "18080:18080"; got != want {
		t.Errorf("ports[0].port = %q, want %q (fragment must win)\n%s", got, want, out)
	}
}

// TestSpliceK3dPorts_DisjointEntriesAllSurvive covers the no-collision
// case: scaffolded YAML declares `18080:80` and `18443:443`, fragment
// declares a disjoint `19190:19190`. The merged output should preserve
// all three entries (nothing to dedupe).
func TestSpliceK3dPorts_DisjointEntriesAllSurvive(t *testing.T) {
	user := []byte(`apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: disjoint
ports:
  - port: 18080:80
    nodeFilters:
      - loadbalancer
  - port: 18443:443
    nodeFilters:
      - loadbalancer
`)
	fragment := []byte(`ports:
  - port: 19190:19190
    nodeFilters:
      - loadbalancer
`)
	out, err := spliceK3dPorts(user, fragment)
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parse merged: %v\n%s", err, out)
	}
	ports := parsed["ports"].([]any)
	if len(ports) != 3 {
		t.Errorf("merged ports[] count = %d, want 3 (disjoint hosts)\n%s", len(ports), out)
	}
	// Spot-check that all three host ports are present.
	got := map[string]bool{}
	for _, p := range ports {
		if m, ok := p.(map[string]any); ok {
			if s, ok := m["port"].(string); ok {
				got[s] = true
			}
		}
	}
	for _, want := range []string{"18080:80", "18443:443", "19190:19190"} {
		if !got[want] {
			t.Errorf("merged ports[] missing %q; got = %v\n%s", want, got, out)
		}
	}
}

// TestSpliceK3dPorts_BothEmpty covers the degenerate case — empty
// user YAML and empty fragment. Splice must succeed and return the
// user YAML verbatim (empty-fragment fast path).
func TestSpliceK3dPorts_BothEmpty(t *testing.T) {
	out, err := spliceK3dPorts(nil, nil)
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	// nil userYAML in -> empty bytes out (the empty-fragment fast path
	// returns userYAML verbatim, which is nil).
	if len(out) != 0 {
		t.Errorf("expected empty output for empty inputs; got %q", out)
	}
}

// TestIngressPinnedVersions parses the embedded VERSION file and
// asserts both keys (envoy_gateway, gateway_api) are present + look
// like vX.Y.Z tags.
func TestIngressPinnedVersions(t *testing.T) {
	envoyVer, gatewayAPIVer, err := ingressPinnedVersions()
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	for _, ver := range []string{envoyVer, gatewayAPIVer} {
		if !strings.HasPrefix(ver, "v") || strings.Count(ver, ".") < 2 {
			t.Errorf("version %q doesn't look like a vX.Y.Z tag", ver)
		}
	}
}

// TestGatewayAPICRDsURL pins the URL shape — the release download
// URL is contract: if upstream relocates the file, forge breaks at
// cluster up. Catching the URL drift in tests gives us a flag
// instead of a runtime 404.
func TestGatewayAPICRDsURL(t *testing.T) {
	got := gatewayAPICRDsURL("v1.5.1")
	want := "https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml"
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

// TestEnvoyGatewayChartRef pins the gateway-helm chart coordinates the
// local install shells `helm` against — the SAME release name/namespace
// the cloud envs run. Drift here means a laptop install lands somewhere
// the cloud doesn't, breaking the "one controller everywhere" invariant.
func TestEnvoyGatewayChartRef(t *testing.T) {
	if envoyGatewayChartRef != "oci://docker.io/envoyproxy/gateway-helm" {
		t.Errorf("chart ref = %q, want the gateway-helm OCI ref", envoyGatewayChartRef)
	}
	if envoyGatewayReleaseName != "eg" {
		t.Errorf("release name = %q, want eg", envoyGatewayReleaseName)
	}
	if envoyGatewayNamespace != "envoy-gateway-system" {
		t.Errorf("namespace = %q, want envoy-gateway-system", envoyGatewayNamespace)
	}
}

// TestEgGatewayClassVendored asserts the vendored GatewayClass forge
// applies after the controller install names the `eg` class and the
// Envoy Gateway controllerName — the class every forge env's Gateways
// reference via the schema default.
func TestEgGatewayClassVendored(t *testing.T) {
	out, err := templates.IngressTemplates().Get("envoy/gatewayclass.yaml")
	if err != nil {
		t.Fatalf("load vendored GatewayClass: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"kind: GatewayClass",
		"name: eg",
		"controllerName: gateway.envoyproxy.io/gatewayclass-controller",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("vendored GatewayClass missing %q", want)
		}
	}
}
