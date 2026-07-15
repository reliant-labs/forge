package generator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/buildinfo"
)

// writeSiblingForgePkg lays out <parent>/forge/pkg/go.mod with the given
// module path and returns the project dir <parent>/myproject.
func writeSiblingForgePkg(t *testing.T, module string) (projectDir, pkgDir string) {
	t.Helper()
	parent := t.TempDir()
	pkgDir = filepath.Join(parent, "forge", "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir sibling pkg: %v", err)
	}
	gomod := "module " + module + "\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatalf("write sibling go.mod: %v", err)
	}
	projectDir = filepath.Join(parent, "myproject")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	return projectDir, pkgDir
}

func TestResolveForgePkgDep_ReleasePinWinsOverSibling(t *testing.T) {
	t.Cleanup(func() { buildinfo.SetPkgVersion("") })
	buildinfo.SetPkgVersion("v0.3.0")

	// Even with a perfectly valid sibling checkout on disk, a stamped
	// release binary pins the published version — release scaffolds
	// must be reproducible regardless of the host's directory layout.
	projectDir, _ := writeSiblingForgePkg(t, "github.com/reliant-labs/forge/pkg")
	pin, dev := resolveForgePkgDep(projectDir)
	if pin != "v0.3.0" || dev != "" {
		t.Errorf("resolveForgePkgDep = (%q, %q), want (v0.3.0, \"\")", pin, dev)
	}
}

func TestResolveForgePkgDep_DevWithSibling(t *testing.T) {
	t.Cleanup(func() { buildinfo.SetPkgVersion("") })
	buildinfo.SetPkgVersion("") // dev build

	projectDir, pkgDir := writeSiblingForgePkg(t, "github.com/reliant-labs/forge/pkg")
	pin, dev := resolveForgePkgDep(projectDir)
	if pin != "" || dev != pkgDir {
		t.Errorf("resolveForgePkgDep = (%q, %q), want (\"\", %q)", pin, dev, pkgDir)
	}
}

func TestResolveForgePkgDep_DevNoSibling(t *testing.T) {
	t.Cleanup(func() { buildinfo.SetPkgVersion(""); buildinfo.ClearPkgModuleVersion() })
	buildinfo.SetPkgVersion("")
	// Force the build-info fallback to empty (a local go.work build, where
	// forge/pkg resolves to "(devel)"): with no release stamp, no sibling,
	// and no build-info version, nothing is emitted and `go mod tidy`
	// resolves from the proxy.
	buildinfo.SetPkgModuleVersion("")

	projectDir := filepath.Join(t.TempDir(), "myproject")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pin, dev := resolveForgePkgDep(projectDir)
	if pin != "" || dev != "" {
		t.Errorf("resolveForgePkgDep = (%q, %q), want both empty", pin, dev)
	}
}

// The daemon / `go install` flow: no release stamp and no sibling checkout,
// but the forge binary was built against a concrete forge/pkg pseudo-version
// recorded in its build info. That pseudo-version must be pinned (as a
// require, no replace) so the gen/ submodule's `go mod tidy` can resolve
// forge/pkg instead of the unresolvable v0.0.0 the templates used to emit.
func TestResolveForgePkgDep_DevNoSibling_BuildInfoPin(t *testing.T) {
	t.Cleanup(func() { buildinfo.SetPkgVersion(""); buildinfo.ClearPkgModuleVersion() })
	buildinfo.SetPkgVersion("")
	const pseudo = "v0.0.0-20260624040937-ce5dfbd929ed"
	buildinfo.SetPkgModuleVersion(pseudo)

	projectDir := filepath.Join(t.TempDir(), "myproject")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pin, dev := resolveForgePkgDep(projectDir)
	if pin != pseudo || dev != "" {
		t.Errorf("resolveForgePkgDep = (%q, %q), want (%q, \"\")", pin, dev, pseudo)
	}
}

// A sibling checkout must still win over the build-info pseudo-version: local
// forge/pkg edits should flow into the scaffold via the live replace, not a
// frozen pin.
func TestResolveForgePkgDep_SiblingWinsOverBuildInfo(t *testing.T) {
	t.Cleanup(func() { buildinfo.SetPkgVersion(""); buildinfo.ClearPkgModuleVersion() })
	buildinfo.SetPkgVersion("")
	buildinfo.SetPkgModuleVersion("v0.0.0-20260624040937-ce5dfbd929ed")

	projectDir, pkgDir := writeSiblingForgePkg(t, "github.com/reliant-labs/forge/pkg")
	pin, dev := resolveForgePkgDep(projectDir)
	if pin != "" || dev != pkgDir {
		t.Errorf("resolveForgePkgDep = (%q, %q), want (\"\", %q)", pin, dev, pkgDir)
	}
}

// A directory that merely happens to be named forge/pkg but declares a
// different module must NOT be wired up as the dev replace target —
// silently vendoring the wrong code is worse than no wiring.
func TestResolveForgePkgDep_WrongModuleSiblingIgnored(t *testing.T) {
	t.Cleanup(func() { buildinfo.SetPkgVersion(""); buildinfo.ClearPkgModuleVersion() })
	buildinfo.SetPkgVersion("")
	buildinfo.SetPkgModuleVersion("") // no build-info fallback for this case

	projectDir, _ := writeSiblingForgePkg(t, "github.com/someone-else/forge/pkg")
	pin, dev := resolveForgePkgDep(projectDir)
	if pin != "" || dev != "" {
		t.Errorf("resolveForgePkgDep = (%q, %q), want both empty for wrong-module sibling", pin, dev)
	}
}
