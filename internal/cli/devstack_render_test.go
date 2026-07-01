package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/devstack"
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
	return renderPort(t, kdir, src, dArgs)
}

// renderAllocatePort renders a one-line KCL module that calls the new
// forge.allocate_port(base, key) builtin through the real plugin seam.
func renderAllocatePort(t *testing.T, kdir string, base int, key string, dArgs []string) int {
	t.Helper()
	src := "import kcl_plugin.forge\n" +
		"port = forge.allocate_port(" + itoa(base) + ", \"" + key + "\")\n"
	return renderPort(t, kdir, src, dArgs)
}

func renderPort(t *testing.T, kdir, src string, dArgs []string) int {
	t.Helper()
	if err := os.WriteFile(filepath.Join(kdir, "main.k"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := kclrender.Run(kdir, kdir, dArgs)
	if err != nil {
		t.Fatalf("render: %v", err)
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

// TestAllocatePortDefaultKeyRendersBase: forge.allocate_port(base, "")
// through the real plugin seam returns base unchanged — the byte-identical
// default.
func TestAllocatePortDefaultKeyRendersBase(t *testing.T) {
	proj := t.TempDir()
	kdir := t.TempDir()
	kclplugin.UseBlockAllocator(func(base int, key string) (int, error) {
		return devstack.AllocatePort(proj, base, key)
	})
	defer kclplugin.UseBlockAllocator(nil)

	if p := renderAllocatePort(t, kdir, 28080, "", nil); p != 28080 {
		t.Errorf("allocate_port(28080, \"\") = %d, want 28080", p)
	}
}

// TestAllocatePortDistinctKeysDisjointBlocks: two keys land in disjoint
// blocks; both ports for a key share the key's block (same +100 shift); and
// the SAME key resolves identically across two renders (up == deploy).
func TestAllocatePortDistinctKeysDisjointBlocks(t *testing.T) {
	proj := t.TempDir()
	kdir := t.TempDir()
	kclplugin.UseBlockAllocator(func(base int, key string) (int, error) {
		return devstack.AllocatePort(proj, base, key)
	})
	defer kclplugin.UseBlockAllocator(nil)

	// key wt-a → block 1.
	gwA := renderAllocatePort(t, kdir, 28080, "wt-a", nil)
	apiA := renderAllocatePort(t, kdir, 3091, "wt-a", nil)
	if gwA != 28180 || apiA != 3191 {
		t.Errorf("wt-a ports = (%d, %d), want (28180, 3191) — one block per key", gwA, apiA)
	}
	// key wt-b → a disjoint block 2.
	gwB := renderAllocatePort(t, kdir, 28080, "wt-b", nil)
	if gwB != 28280 {
		t.Errorf("wt-b gateway = %d, want 28280 (disjoint block)", gwB)
	}
	// up == deploy: a fresh render of the SAME key reads the persisted block.
	again := renderAllocatePort(t, kdir, 28080, "wt-a", nil)
	if again != gwA {
		t.Errorf("up vs deploy drift for wt-a: %d != %d", again, gwA)
	}
}

// TestUpDeployResolveIdenticalPort is the regression-lock for the
// up-vs-deploy port drift on the GENERAL resolve_port primitive (kept
// alongside allocate_port): once the port store is armed at a path,
// resolve_port returns the SAME port whether the render is up or deploy.
func TestUpDeployResolveIdenticalPort(t *testing.T) {
	proj := t.TempDir()
	kdir := t.TempDir()
	storePath := filepath.Join(proj, ".forge", "ports-dev.json")

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

// TestGitFactsReachKCL proves option("worktree") and option("branch") are
// visible inside the render — the user's hard requirement (push the raw git
// facts INTO KCL and let the author key on whichever they want).
func TestGitFactsReachKCL(t *testing.T) {
	kdir := t.TempDir()
	src := `wt = option("worktree")
br = option("branch")
key = wt or ""
suffix = "-" + key if key else ""
ns = "control-plane-dev" + suffix
`
	if err := os.WriteFile(filepath.Join(kdir, "main.k"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := devstack.Options{Worktree: "wt-feat", Branch: "feature-x"}
	out, err := kclrender.Run(kdir, kdir, opts.DArgs())
	if err != nil {
		t.Fatalf("render with git facts: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	if got := m["wt"]; got != "wt-feat" {
		t.Errorf("option(\"worktree\") = %v, want wt-feat", got)
	}
	if got := m["br"]; got != "feature-x" {
		t.Errorf("option(\"branch\") = %v, want feature-x", got)
	}
	if got := m["ns"]; got != "control-plane-dev-wt-feat" {
		t.Errorf("namespace composition = %v, want control-plane-dev-wt-feat", got)
	}
}

// TestDefaultGitFactsRenderByteIdentical proves the default stack emits NO
// options, so option("worktree") is None (KCL default) and the namespace
// composition collapses to today's single-stack value.
func TestDefaultGitFactsRenderByteIdentical(t *testing.T) {
	kdir := t.TempDir()
	src := `wt = option("worktree") or ""
key = wt
suffix = "-" + key if key else ""
ns = "control-plane-dev" + suffix
`
	if err := os.WriteFile(filepath.Join(kdir, "main.k"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := kclrender.Run(kdir, kdir, devstack.Options{}.DArgs())
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
}
