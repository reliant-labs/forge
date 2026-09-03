package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/kclvendor"
)

// kclLegacyGitTagMod is the shape older scaffolds emitted — a git tag
// that was never published. Every build of forge must heal it.
const kclLegacyGitTagMod = `[package]
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

// TestSyncForgeKCL_VendorsBothCandidates: the sync swaps the dead git
// tag in BOTH managed kcl.mod locations to depth-correct relative paths
// and materializes the embedded module.
func TestSyncForgeKCL_VendorsBothCandidates(t *testing.T) {
	dir := t.TempDir()
	deployMod := writeProjectKclMod(t, dir, "deploy/kcl/kcl.mod", kclLegacyGitTagMod)
	rootMod := writeProjectKclMod(t, dir, "kcl.mod", kclLegacyGitTagMod)

	if err := syncForgeKCL(dir, false); err != nil {
		t.Fatalf("syncForgeKCL: %v", err)
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
	if err := syncForgeKCL(dir, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	after, _ := os.ReadFile(deployMod)
	if string(before) != string(after) {
		t.Errorf("second sync changed kcl.mod bytes")
	}
}

// TestSyncForgeKCL_NoKclModNeverOrphansVendorDir: with no managed
// kcl.mod referencing the vendor, nothing is materialized.
func TestSyncForgeKCL_NoKclModNeverOrphansVendorDir(t *testing.T) {
	dir := t.TempDir()
	if err := syncForgeKCL(dir, false); err != nil {
		t.Fatalf("syncForgeKCL: %v", err)
	}
	if kclvendor.Present(dir) {
		t.Errorf(".forge-kcl/ materialized with nothing referencing it")
	}
}

// TestSyncForgeKCL_ReleaseBuildDoesNotBreakAWorkingProject is the
// regression that motivated always-vendoring.
//
// A release build (stamped pkg version) used to take the un-vendor
// direction: it DELETED a working .forge-kcl/ and rewrote the dependency
// to a `kcl-vX.Y.Z` git tag that had never been published, so upgrading
// forge broke every project it touched. There is no longer a release
// direction — a stamped build must leave a working project working.
//
// NOT parallel: buildinfo is process-global.
func TestSyncForgeKCL_ReleaseBuildDoesNotBreakAWorkingProject(t *testing.T) {
	dir := t.TempDir()
	deployMod := writeProjectKclMod(t, dir, "deploy/kcl/kcl.mod", kclLegacyGitTagMod)
	if err := syncForgeKCL(dir, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if !kclvendor.Present(dir) {
		t.Fatalf("precondition: vendor dir missing")
	}
	working, _ := os.ReadFile(deployMod)

	buildinfo.SetPkgVersion("v9.9.9")
	defer buildinfo.SetPkgVersion("")
	if err := syncForgeKCL(dir, false); err != nil {
		t.Fatalf("release sync: %v", err)
	}

	if !kclvendor.Present(dir) {
		t.Fatalf("release build deleted the vendored module — the upgrade regression is back")
	}
	got, _ := os.ReadFile(deployMod)
	if strings.Contains(string(got), "git = ") {
		t.Errorf("release build rewrote the dependency to a git tag:\n%s", got)
	}
	if !strings.Contains(string(got), `forge = { path = "../../.forge-kcl" }`) {
		t.Errorf("release build did not leave the vendored dep in place:\n%s", got)
	}
	if string(got) != string(working) {
		t.Errorf("release build churned kcl.mod:\n--- before ---\n%s\n--- after ---\n%s", working, got)
	}
}

// TestSyncForgeKCL_ReleaseBuildVendorsAFreshProject: a stamped release
// build vendors just like any other build. Previously it emitted the
// unresolvable published tag instead.
func TestSyncForgeKCL_ReleaseBuildVendorsAFreshProject(t *testing.T) {
	dir := t.TempDir()
	deployMod := writeProjectKclMod(t, dir, "deploy/kcl/kcl.mod", kclLegacyGitTagMod)

	buildinfo.SetPkgVersion("v9.9.9")
	defer buildinfo.SetPkgVersion("")
	if err := syncForgeKCL(dir, false); err != nil {
		t.Fatalf("release sync: %v", err)
	}
	if !kclvendor.Present(dir) {
		t.Fatalf("release build did not materialize %s/", kclvendor.VendorDirName)
	}
	got, _ := os.ReadFile(deployMod)
	if !strings.Contains(string(got), `forge = { path = "../../.forge-kcl" }`) {
		t.Errorf("release build did not vendor the dependency:\n%s", got)
	}
}

// TestSyncForgeKCL_UnmanagedShapeIsLeftAlone: a kcl.mod carrying a forge
// dependency in a shape forge does not manage is never edited.
func TestSyncForgeKCL_UnmanagedShapeIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	unmanaged := `[package]
name = "p"

[dependencies]
forge = { path = "../../.forge-kcl", version = "0.1.0" }
`
	modPath := writeProjectKclMod(t, dir, "deploy/kcl/kcl.mod", unmanaged)
	if err := syncForgeKCL(dir, false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got, _ := os.ReadFile(modPath)
	if string(got) != unmanaged {
		t.Errorf("unmanaged kcl.mod was edited:\n%s", got)
	}
}
