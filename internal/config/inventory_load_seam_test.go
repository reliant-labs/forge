package config

// inventory_load_seam_test.go — the load seam owns the frontend
// inventory, so every command that loads a forge.yaml gets one answer.

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSeamProject(t *testing.T, forgeYAML string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "forge.yaml"), []byte(forgeYAML), 0o600); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	return root
}

func mkSeamFrontendDir(t *testing.T, root, name, marker string) {
	t.Helper()
	dir := filepath.Join(root, "frontends", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	if marker == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, marker), []byte("export default {}\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

// TestLoadResolvesInventoryWithMarkerGate is §3 items 1 and 2: lint's
// typecheck lane and its dotenv rule used to reach the right frontend by
// ACCIDENT, via a bare directory scan with no marker gate. A scratch
// directory was therefore typechecked by lint and ignored by generate.
//
// Resolving at the load seam means both commands read the same
// marker-gated inventory, so the stray directory is excluded everywhere.
func TestLoadResolvesInventoryWithMarkerGate(t *testing.T) {
	root := writeSeamProject(t, "name: demo\nmodule_path: github.com/example/demo\n")
	mkSeamFrontendDir(t, root, "console", "next.config.ts")
	mkSeamFrontendDir(t, root, "scratch", "") // a half-deleted tree: no marker

	cfg, err := LoadProjectDir(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Frontends) != 1 {
		t.Fatalf("resolved %d frontend(s), want 1: %+v", len(cfg.Frontends), cfg.Frontends)
	}
	if cfg.Frontends[0].Name != "console" {
		t.Errorf("resolved %q, want console", cfg.Frontends[0].Name)
	}
	for _, fe := range cfg.Frontends {
		if fe.Name == "scratch" {
			t.Error("a directory with no framework marker must not enter the inventory — " +
				"generate has always excluded it, and lint must agree")
		}
	}
}

// TestLoadDerivedInventoryEnablesFrontendFeature is §3 item 4 and the
// gate question: features.frontend is derived from the inventory being
// non-empty. Resolving the inventory BEFORE feature derivation is what
// keeps the frontend pipeline steps (and gateFrontendHasFrontends)
// ungated for a project that declares no `frontends:` block.
func TestLoadDerivedInventoryEnablesFrontendFeature(t *testing.T) {
	root := writeSeamProject(t, "name: demo\nmodule_path: github.com/example/demo\n")
	mkSeamFrontendDir(t, root, "console", "next.config.ts")
	// features.frontend also requires codegen, which derives from the
	// project being SERVICE-shaped. control-plane is; give the fixture the
	// same shape so this test measures the inventory, not the kind.
	if err := os.MkdirAll(filepath.Join(root, "internal", "handlers"), 0o755); err != nil {
		t.Fatalf("mkdir handlers: %v", err)
	}

	cfg, err := LoadProjectDir(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Features.FrontendEnabled() {
		t.Error("features.frontend must be on for a project whose inventory resolved to a real " +
			"frontend; otherwise every frontend generate step is gated off")
	}
}

// TestLoadDeclaredBlockWinsOverDiscovery pins the ordering that keeps
// this invisible to projects that already declare their frontends: an
// explicit block is returned untouched and the scan never runs, so
// discovery cannot add a frontend an author deliberately left out.
func TestLoadDeclaredBlockWinsOverDiscovery(t *testing.T) {
	root := writeSeamProject(t, "name: demo\nmodule_path: github.com/example/demo\n"+
		"frontends:\n  - name: admin\n    type: nextjs\n    path: apps/admin\n")
	mkSeamFrontendDir(t, root, "console", "next.config.ts") // present, but undeclared

	cfg, err := LoadProjectDir(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Frontends) != 1 || cfg.Frontends[0].Name != "admin" {
		t.Fatalf("declared block must win untouched, got %+v", cfg.Frontends)
	}
	if cfg.Frontends[0].DeclaredDir() != "apps/admin" {
		t.Errorf("declared path = %q, want apps/admin preserved verbatim", cfg.Frontends[0].DeclaredDir())
	}
}

// TestLoadKeepsSourcedFrontendPathEmpty is the second mutation's rule at
// the load seam: a cross-repo frontend gets NO invented directory.
func TestLoadKeepsSourcedFrontendPathEmpty(t *testing.T) {
	root := writeSeamProject(t, "name: demo\nmodule_path: github.com/example/demo\n"+
		"frontends:\n  - name: sibling-web\n    type: vite-spa\n"+
		"    source:\n      repo: github.com/example/sibling\n      ref: v1.0.0\n")

	cfg, err := LoadProjectDir(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Frontends[0].DeclaredDir(); got != "" {
		t.Errorf("path = %q, want empty — a frontend whose code is in another repository has no "+
			"directory in this tree until its source is materialized", got)
	}
}
