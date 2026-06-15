package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
// asserts both keys are present + look like vX.Y.Z tags.
func TestIngressPinnedVersions(t *testing.T) {
	traefikVer, gatewayAPIVer, err := ingressPinnedVersions()
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	for _, ver := range []string{traefikVer, gatewayAPIVer} {
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
	got := gatewayAPICRDsURL("v1.2.0")
	want := "https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml"
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

// setupTraefikEntrypointFixture writes deploy/kcl/dev (just enough
// for the stat-check in collectTraefikEntrypoints) and a JSON
// RenderKCL fixture under FORGE_KCL_RENDER_FIXTURE that decodes to
// the given gateways/listeners shape.
func setupTraefikEntrypointFixture(t *testing.T, renderJSON string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "deploy", "kcl", "dev"), 0o755); err != nil {
		t.Fatalf("mkdir dev kcl: %v", err)
	}
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, renderJSON))
	return dir
}

// TestCollectTraefikEntrypoints_ProjectsListeners is the happy path —
// two gateways, one listener each, emits two entrypoints in
// (port-ascending) order with the listener's name carried through.
func TestCollectTraefikEntrypoints_ProjectsListeners(t *testing.T) {
	dir := setupTraefikEntrypointFixture(t, `{
		"gateways": [
			{"name": "public",  "listeners": [{"name": "http", "port": 18080, "protocol": "HTTP"}]},
			{"name": "private", "listeners": [{"name": "grpc", "port": 19190, "protocol": "H2C"}]}
		]
	}`)
	got, err := collectTraefikEntrypoints(context.Background(), dir)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	want := []traefikEntrypoint{
		{Name: "http", Port: 18080, Protocol: "HTTP"},
		{Name: "grpc", Port: 19190, Protocol: "H2C"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %+v)", len(got), len(want), got)
	}
	for i, e := range got {
		if e != want[i] {
			t.Errorf("entrypoint[%d] = %+v, want %+v", i, e, want[i])
		}
	}
}

// TestCollectTraefikEntrypoints_PortDedupe — two gateways collide on a
// port. Lower-sorted (port, gateway, name) wins; the duplicate is
// dropped. Mirrors the k3d-ports generator's first-wins policy so the
// two outputs stay in lockstep.
func TestCollectTraefikEntrypoints_PortDedupe(t *testing.T) {
	dir := setupTraefikEntrypointFixture(t, `{
		"gateways": [
			{"name": "b-gw", "listeners": [{"name": "http", "port": 18080, "protocol": "HTTP"}]},
			{"name": "a-gw", "listeners": [{"name": "http", "port": 18080, "protocol": "HTTP"}]}
		]
	}`)
	got, err := collectTraefikEntrypoints(context.Background(), dir)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 entrypoint after dedupe, got %d (%+v)", len(got), got)
	}
	if got[0].Port != 18080 || got[0].Name != "http" {
		t.Errorf("expected http:18080, got %+v", got[0])
	}
}

// TestCollectTraefikEntrypoints_NameCollisionDistinctPorts — two
// gateways both name a listener "http" on different ports. Traefik
// requires entrypoint names to be unique, so the second one gets a
// "-2" suffix. The lowest port keeps the bare name.
func TestCollectTraefikEntrypoints_NameCollisionDistinctPorts(t *testing.T) {
	dir := setupTraefikEntrypointFixture(t, `{
		"gateways": [
			{"name": "public",  "listeners": [{"name": "http", "port": 18080, "protocol": "HTTP"}]},
			{"name": "private", "listeners": [{"name": "http", "port": 18081, "protocol": "HTTP"}]}
		]
	}`)
	got, err := collectTraefikEntrypoints(context.Background(), dir)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entrypoints, got %d (%+v)", len(got), got)
	}
	if got[0].Name != "http" || got[0].Port != 18080 {
		t.Errorf("entrypoint[0] = %+v, want http:18080", got[0])
	}
	if got[1].Name != "http-2" || got[1].Port != 18081 {
		t.Errorf("entrypoint[1] = %+v, want http-2:18081", got[1])
	}
}

// TestCollectTraefikEntrypoints_NoDevKCL — a project with no
// deploy/kcl/dev directory (e.g. just-scaffolded, features.ingress
// still off) returns nil/nil. Cluster-up must still succeed.
func TestCollectTraefikEntrypoints_NoDevKCL(t *testing.T) {
	dir := t.TempDir() // intentionally no deploy/kcl/dev
	got, err := collectTraefikEntrypoints(context.Background(), dir)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil entrypoints, got %+v", got)
	}
}

// TestCollectTraefikEntrypoints_NoGateways — dev KCL is present but
// the project hasn't authored any gateways. Returns nil/nil so the
// install applies with only the default ping entrypoint.
func TestCollectTraefikEntrypoints_NoGateways(t *testing.T) {
	dir := setupTraefikEntrypointFixture(t, `{"gateways": []}`)
	got, err := collectTraefikEntrypoints(context.Background(), dir)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 entrypoints, got %+v", got)
	}
}

// TestRenderTraefikInstall_NoEntrypoints — passing nil entrypoints
// must still produce parseable YAML with the static defaults.
func TestRenderTraefikInstall_NoEntrypoints(t *testing.T) {
	out, err := renderTraefikInstall(nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(out), "--providers.kubernetesgateway=true") {
		t.Error("expected providers.kubernetesgateway arg in rendered output")
	}
	// The template's header comment mentions `--entrypoints.<name>.address`;
	// look for the rendered arg form (- --entrypoints.) to avoid matching it.
	if strings.Contains(string(out), "- --entrypoints.") {
		t.Error("expected no --entrypoints args when entrypoints is nil")
	}
}

// TestRenderTraefikInstall_EmitsEntrypointArgs — given two entrypoints
// the rendered output carries the matching --entrypoints.<name>.address
// args, the container ports, and the Service ports.
func TestRenderTraefikInstall_EmitsEntrypointArgs(t *testing.T) {
	out, err := renderTraefikInstall([]traefikEntrypoint{
		{Name: "http", Port: 18080, Protocol: "HTTP"},
		{Name: "grpc", Port: 19190, Protocol: "H2C"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"--entrypoints.http.address=:18080",
		"--entrypoints.grpc.address=:19190",
		"containerPort: 18080",
		"containerPort: 19190",
		"port: 18080",
		"port: 19190",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered output missing %q", want)
		}
	}
}
