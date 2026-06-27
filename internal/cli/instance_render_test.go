package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/instance"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
)

// renderResolvePort renders a one-line KCL module that resolves a port for
// `role` (preferred `pref`) through the real plugin seam, returning the
// resolved port. It mirrors what both `forge up` and `forge deploy` do once
// they've armed the port store.
func renderResolvePort(t *testing.T, kdir, role string, pref int, dArgs []string) int {
	t.Helper()
	src := "import kcl_plugin.forge\n" +
		"port = forge.resolve_port(\"" + role + "\", " + itoa(pref) + ")\n"
	if err := os.WriteFile(filepath.Join(kdir, "main.k"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := kclrender.Run(kdir, kdir, dArgs)
	if err != nil {
		t.Fatalf("render resolve_port: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	p, ok := m["port"].(float64)
	if !ok {
		t.Fatalf("no port in render output: %s", out)
	}
	return int(p)
}

func itoa(i int) string {
	// Tiny local helper to avoid pulling strconv into a test that needs one int.
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TestUpDeployResolveIdenticalPort is the regression-lock for the
// up-vs-deploy port drift: once the port store is armed at a path,
// resolve_port returns the SAME port whether the render is the up render or
// the deploy render. Previously `forge up` armed the store and `forge
// deploy` re-probed preferred ports → divergence (the reliant-api 3091
// hand-pin). Now both read the one persisted store.
func TestUpDeployResolveIdenticalPort(t *testing.T) {
	proj := t.TempDir()
	kdir := t.TempDir()
	storePath := instance.PortStorePath(proj, "dev", instance.Instance{})

	// `forge up`: arm the store, render, allocation persists.
	restore := kclplugin.UsePortStore(storePath)
	upPort := renderResolvePort(t, kdir, "reliant-api", 3091, nil)
	_ = restore // up commits the render; restore only used on rejection

	// `forge deploy`: arm the SAME store path (fresh resolver, reads the
	// persisted file), render. Must land on the identical port.
	kclplugin.UsePortStore(storePath)
	deployPort := renderResolvePort(t, kdir, "reliant-api", 3091, nil)

	if upPort != deployPort {
		t.Fatalf("up vs deploy port drift: up=%d deploy=%d (store=%s)", upPort, deployPort, storePath)
	}

	// And the persisted store actually recorded it (single source of truth).
	data, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("port store not written: %v", err)
	}
	var stored map[string]int
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["reliant-api"] != upPort {
		t.Errorf("store has reliant-api=%d, render resolved %d", stored["reliant-api"], upPort)
	}
}

// TestInstanceScopedStoresIsolate proves two instances render into distinct
// store files and never collide on a port for the same role.
func TestInstanceScopedStoresIsolate(t *testing.T) {
	proj := t.TempDir()
	kdir := t.TempDir()

	a := instance.Instance{Name: "wt-a", Index: 1}
	b := instance.Instance{Name: "wt-b", Index: 2}
	pathA := instance.PortStorePath(proj, "dev", a)
	pathB := instance.PortStorePath(proj, "dev", b)
	if pathA == pathB {
		t.Fatal("instance-scoped store paths collided")
	}

	// The contract: KCL derives the per-instance PREFERRED port from
	// option("instance_index") (base + idx*100) and hands THAT to
	// resolve_port; the instance-scoped store then stabilizes it across
	// up/deploy runs. So each instance's gateway starts from a distinct
	// preferred port — the store files keep the two blocks independent.
	kclplugin.UsePortStore(pathA)
	portA := renderResolvePort(t, kdir, "gateway", 28080+a.Index*100, a.DArgs())

	kclplugin.UsePortStore(pathB)
	portB := renderResolvePort(t, kdir, "gateway", 28080+b.Index*100, b.DArgs())

	// Distinct store files exist (instance-scoped, never share a block).
	if _, err := os.Stat(pathA); err != nil {
		t.Errorf("instance A store missing: %v", err)
	}
	if _, err := os.Stat(pathB); err != nil {
		t.Errorf("instance B store missing: %v", err)
	}
	// Each instance's gateway lands in its own block — no collision.
	if portA == portB {
		t.Errorf("two instances got the SAME port %d for role gateway", portA)
	}
	if portA != 28180 {
		t.Errorf("instance A gateway = %d, want 28180 (28080 + 1*100)", portA)
	}
	if portB != 28280 {
		t.Errorf("instance B gateway = %d, want 28280 (28080 + 2*100)", portB)
	}
}

// TestInstanceOptionsReachKCL proves option("instance") and
// option("instance_index") are visible inside the render — the user's hard
// requirement (push instance identity INTO KCL).
func TestInstanceOptionsReachKCL(t *testing.T) {
	kdir := t.TempDir()
	src := `name = option("instance")
idx = option("instance_index")
suffix = "-" + name if name else ""
ns = "control-plane-dev" + suffix
block_base = 28080 + idx * 100
`
	if err := os.WriteFile(filepath.Join(kdir, "main.k"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	inst := instance.Instance{Name: "feat-x", Index: 2}
	out, err := kclrender.Run(kdir, kdir, inst.DArgs())
	if err != nil {
		t.Fatalf("render with instance options: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	if got := m["name"]; got != "feat-x" {
		t.Errorf("option(\"instance\") = %v, want feat-x", got)
	}
	if got := m["idx"].(float64); int(got) != 2 {
		t.Errorf("option(\"instance_index\") = %v, want 2", got)
	}
	if got := m["ns"]; got != "control-plane-dev-feat-x" {
		t.Errorf("namespace composition = %v, want control-plane-dev-feat-x", got)
	}
	if got := m["block_base"].(float64); int(got) != 28280 {
		t.Errorf("port block base = %v, want 28280 (28080 + 2*100)", int(got))
	}
}

// TestDefaultInstanceRendersByteIdentical proves the default instance emits
// NO instance options, so option("instance") is None (KCL default) and the
// namespace/port composition collapses to today's single-stack values.
func TestDefaultInstanceRendersByteIdentical(t *testing.T) {
	kdir := t.TempDir()
	src := `name = option("instance") or ""
idx = option("instance_index") or 0
suffix = "-" + name if name else ""
ns = "control-plane-dev" + suffix
block_base = 28080 + idx * 100
`
	if err := os.WriteFile(filepath.Join(kdir, "main.k"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// Default instance => nil dArgs.
	out, err := kclrender.Run(kdir, kdir, instance.Instance{}.DArgs())
	if err != nil {
		t.Fatalf("render default: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["ns"] != "control-plane-dev" {
		t.Errorf("default ns = %v, want control-plane-dev (no suffix)", m["ns"])
	}
	if int(m["block_base"].(float64)) != 28080 {
		t.Errorf("default block base = %v, want 28080 (no offset)", m["block_base"])
	}
}
