package cli

import (
	"context"
	"os"
	"testing"
)

// k3dUseConfig is a k3d Simple-config that references a STANDALONE registry
// via `registries.use`, with the inline containerd mirror carrying the host
// push port on its `localhost:<port>` key — the shape control-plane's
// deploy/k3d.yaml uses.
const k3dUseConfig = `apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: control-plane
registries:
  use:
    - k3d-control-plane-registry:5000
  config: |
    mirrors:
      "registry.localhost:5000":
        endpoint:
          - http://k3d-control-plane-registry:5000
      "localhost:5051":
        endpoint:
          - http://k3d-control-plane-registry:5000
`

// k3dCreateConfig uses `registries.create` (cluster-owned) — forge must NOT
// ensure a standalone registry for it (k3d creates that registry itself).
const k3dCreateConfig = `apiVersion: k3d.io/v1alpha5
kind: Simple
metadata:
  name: legacy
registries:
  create:
    name: legacy-registry
    hostPort: "5051"
`

// TestParseUseRegistries pins that a `registries.use` config yields the
// registry name (internal :5000 stripped) with the host port read from the
// `localhost:<port>` mirror key.
func TestParseUseRegistries(t *testing.T) {
	refs, err := parseUseRegistries([]byte(k3dUseConfig))
	if err != nil {
		t.Fatalf("parseUseRegistries: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs: got %d want 1", len(refs))
	}
	if refs[0].Name != "k3d-control-plane-registry" {
		t.Errorf("Name = %q want k3d-control-plane-registry (internal :5000 stripped)", refs[0].Name)
	}
	if refs[0].HostPort != 5051 {
		t.Errorf("HostPort = %d want 5051 (from the localhost:5051 mirror key)", refs[0].HostPort)
	}
}

// TestParseUseRegistries_CreateIsNoop confirms a `registries.create` config
// yields no refs — forge leaves cluster-owned registries to k3d.
func TestParseUseRegistries_CreateIsNoop(t *testing.T) {
	refs, err := parseUseRegistries([]byte(k3dCreateConfig))
	if err != nil {
		t.Fatalf("parseUseRegistries: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs: got %d want 0 (create is k3d's job, not a standalone ensure)", len(refs))
	}
}

// TestHostPortFromMirrorConfig checks the host port is taken from the
// canonical `localhost:<port>` mirror key (not an arbitrary registry.localhost
// alias), and is 0 when no host:port mirror key is present.
func TestHostPortFromMirrorConfig(t *testing.T) {
	const mirror = `mirrors:
  "registry.localhost:5000":
    endpoint:
      - http://k3d-control-plane-registry:5000
  "localhost:5051":
    endpoint:
      - http://k3d-control-plane-registry:5000
`
	if got := hostPortFromMirrorConfig(mirror); got != 5051 {
		t.Errorf("hostPortFromMirrorConfig = %d want 5051 (localhost key)", got)
	}
	if got := hostPortFromMirrorConfig(""); got != 0 {
		t.Errorf("hostPortFromMirrorConfig(empty) = %d want 0", got)
	}
	// No localhost key, only a registry.localhost alias — fall back to the
	// max parseable host:port (5000 here).
	const aliasOnly = `mirrors:
  "registry.localhost:5000":
    endpoint:
      - http://k3d-control-plane-registry:5000
`
	if got := hostPortFromMirrorConfig(aliasOnly); got != 5000 {
		t.Errorf("hostPortFromMirrorConfig(alias-only) = %d want 5000 (fallback)", got)
	}
}

// TestEnsureStandaloneRegistry_Idempotent asserts a present registry is a
// no-op (no create) and an absent one is created exactly once. Both k3d
// shell-out seams are stubbed so the test never touches k3d.
func TestEnsureStandaloneRegistry_Idempotent(t *testing.T) {
	origExists := registryExistsFn
	origCreate := registryCreateFn
	t.Cleanup(func() {
		registryExistsFn = origExists
		registryCreateFn = origCreate
	})

	// Present -> no create.
	var created []k3dRegistryRef
	registryExistsFn = func(_ context.Context, _ string) (bool, error) { return true, nil }
	registryCreateFn = func(_ context.Context, ref k3dRegistryRef) error {
		created = append(created, ref)
		return nil
	}
	if err := ensureStandaloneRegistry(t.Context(), k3dRegistryRef{Name: "k3d-cp-registry", HostPort: 5051}); err != nil {
		t.Fatalf("ensureStandaloneRegistry(present): %v", err)
	}
	if len(created) != 0 {
		t.Fatalf("present registry triggered %d create(s); want 0", len(created))
	}

	// Absent -> create once, with the parsed name + host port.
	created = nil
	registryExistsFn = func(_ context.Context, _ string) (bool, error) { return false, nil }
	if err := ensureStandaloneRegistry(t.Context(), k3dRegistryRef{Name: "k3d-cp-registry", HostPort: 5051}); err != nil {
		t.Fatalf("ensureStandaloneRegistry(absent): %v", err)
	}
	if len(created) != 1 || created[0].Name != "k3d-cp-registry" || created[0].HostPort != 5051 {
		t.Fatalf("absent registry created %v; want exactly [{k3d-cp-registry 5051}]", created)
	}
}

// TestEnsureConfigRegistries_FromFile drives the full file->ensure path
// against a temp k3d.yaml, asserting the standalone registry is ensured with
// the name + host port parsed from the config.
func TestEnsureConfigRegistries_FromFile(t *testing.T) {
	origExists := registryExistsFn
	origCreate := registryCreateFn
	t.Cleanup(func() {
		registryExistsFn = origExists
		registryCreateFn = origCreate
	})

	path := t.TempDir() + "/k3d.yaml"
	if err := os.WriteFile(path, []byte(k3dUseConfig), 0o644); err != nil {
		t.Fatalf("write temp k3d.yaml: %v", err)
	}

	var created []k3dRegistryRef
	registryExistsFn = func(_ context.Context, _ string) (bool, error) { return false, nil }
	registryCreateFn = func(_ context.Context, ref k3dRegistryRef) error {
		created = append(created, ref)
		return nil
	}
	if err := ensureConfigRegistries(t.Context(), path); err != nil {
		t.Fatalf("ensureConfigRegistries: %v", err)
	}
	if len(created) != 1 || created[0].Name != "k3d-control-plane-registry" || created[0].HostPort != 5051 {
		t.Fatalf("ensured %v; want exactly [{k3d-control-plane-registry 5051}]", created)
	}

	// Empty path is a no-op.
	created = nil
	if err := ensureConfigRegistries(t.Context(), ""); err != nil {
		t.Fatalf("ensureConfigRegistries(empty): %v", err)
	}
	if len(created) != 0 {
		t.Fatalf("empty path ensured %d registr(ies); want 0", len(created))
	}
}
