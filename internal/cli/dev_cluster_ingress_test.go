package cli

import (
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
