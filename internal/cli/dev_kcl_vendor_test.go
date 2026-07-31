package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/kclvendor"
)

// kclScaffoldMod mirrors the published-tag shape kcl.mod.tmpl renders.
const kclScaffoldMod = `[package]
name = "proj-deploy"
edition = "v0.11.0"
version = "0.0.1"

[dependencies]
forge = { git = "https://github.com/reliant-labs/forge.git", tag = "kcl-v0.1.0" }
`

func writeProjectKclMod(t *testing.T, projectDir, rel, content string) string {
	t.Helper()
	path := filepath.Join(projectDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestSyncDevForgeKCL_DevBuildVendorsBothCandidates: a dev build (no
// stamped pkg version) swaps the published tag in BOTH managed kcl.mod
// locations to depth-correct relative paths and materializes the
// embedded module. NOT parallel: buildinfo is process-global.
func TestSyncDevForgeKCL_DevBuildVendorsBothCandidates(t *testing.T) {
	if buildinfo.PkgVersion() != "" {
		t.Skip("test binary built with a stamped pkg version")
	}
	dir := t.TempDir()
	deployMod := writeProjectKclMod(t, dir, "deploy/kcl/kcl.mod", kclScaffoldMod)
	rootMod := writeProjectKclMod(t, dir, "kcl.mod", kclScaffoldMod)

	if err := syncDevForgeKCL(dir); err != nil {
		t.Fatalf("syncDevForgeKCL: %v", err)
	}
	deployGot, _ := os.ReadFile(deployMod)
	if !strings.Contains(string(deployGot), `forge = { path = "../../.forge-kcl" }`) {
		t.Errorf("deploy/kcl/kcl.mod not vendored:\n%s", deployGot)
	}
	rootGot, _ := os.ReadFile(rootMod)
	if !strings.Contains(string(rootGot), `forge = { path = "./.forge-kcl" }`) {
		t.Errorf("root kcl.mod not vendored:\n%s", rootGot)
	}
	if !kclvendor.Present(dir) {
		t.Errorf(".forge-kcl/ not materialized")
	}
	// Idempotent second run.
	before, _ := os.ReadFile(deployMod)
	if err := syncDevForgeKCL(dir); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	after, _ := os.ReadFile(deployMod)
	if string(before) != string(after) {
		t.Errorf("second sync changed kcl.mod bytes")
	}
}

// TestSyncDevForgeKCL_NoKclModNeverOrphansVendorDir: with no managed
// kcl.mod referencing the vendor, nothing is materialized.
func TestSyncDevForgeKCL_NoKclModNeverOrphansVendorDir(t *testing.T) {
	if buildinfo.PkgVersion() != "" {
		t.Skip("test binary built with a stamped pkg version")
	}
	dir := t.TempDir()
	if err := syncDevForgeKCL(dir); err != nil {
		t.Fatalf("syncDevForgeKCL: %v", err)
	}
	if kclvendor.Present(dir) {
		t.Errorf(".forge-kcl/ materialized with nothing referencing it")
	}
}

// TestSyncDevForgeKCL_ReleaseRestoresPublishedAndRemovesVendor: the
// release direction (stamped pkg version) restores the published tag
// in marker-owned blocks and deletes .forge-kcl/ — the same swap
// semantics as .forge-pkg → published pin.
func TestSyncDevForgeKCL_ReleaseRestoresPublishedAndRemovesVendor(t *testing.T) {
	dir := t.TempDir()
	deployMod := writeProjectKclMod(t, dir, "deploy/kcl/kcl.mod", kclScaffoldMod)
	if err := syncDevForgeKCL(dir); err != nil { // dev pass: vendor it
		t.Fatalf("dev sync: %v", err)
	}
	if !kclvendor.Present(dir) {
		t.Fatalf("precondition: vendor dir missing")
	}

	buildinfo.SetPkgVersion("v9.9.9")
	defer buildinfo.SetPkgVersion("")
	if err := syncDevForgeKCL(dir); err != nil {
		t.Fatalf("release sync: %v", err)
	}
	got, _ := os.ReadFile(deployMod)
	if !strings.Contains(string(got), kclvendor.PublishedDepLine) {
		t.Errorf("published dep not restored:\n%s", got)
	}
	if strings.Contains(string(got), kclvendor.MarkerHeader) {
		t.Errorf("marker block lingers:\n%s", got)
	}
	if kclvendor.Present(dir) {
		t.Errorf(".forge-kcl/ not removed on release un-vendor")
	}
}

// TestSyncDevForgeKCL_ReleaseKeepsVendorWhenUnmanagedShapeReferencesIt:
// a kcl.mod that still points at .forge-kcl in a shape forge does not
// manage blocks the directory removal (never delete what someone still
// resolves against).
func TestSyncDevForgeKCL_ReleaseKeepsVendorWhenUnmanagedShapeReferencesIt(t *testing.T) {
	dir := t.TempDir()
	unmanaged := `[package]
name = "p"

[dependencies]
forge = { path = "../../.forge-kcl", version = "0.1.0" }
`
	modPath := writeProjectKclMod(t, dir, "deploy/kcl/kcl.mod", unmanaged)
	if _, err := kclvendor.Materialize(dir); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	buildinfo.SetPkgVersion("v9.9.9")
	defer buildinfo.SetPkgVersion("")
	if err := syncDevForgeKCL(dir); err != nil {
		t.Fatalf("release sync: %v", err)
	}
	if !kclvendor.Present(dir) {
		t.Errorf(".forge-kcl/ deleted while an unmanaged kcl.mod still references it")
	}
	got, _ := os.ReadFile(modPath)
	if string(got) != unmanaged {
		t.Errorf("unmanaged kcl.mod was edited:\n%s", got)
	}
}
