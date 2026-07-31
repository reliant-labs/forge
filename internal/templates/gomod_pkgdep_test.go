// File: internal/templates/gomod_pkgdep_test.go
//
// Pins the forge/pkg dependency block emitted by go.mod.tmpl and
// gen-go.mod.tmpl (see internal/generator/project_pkgdep.go). forge/pkg is
// a PUBLISHED module, so a scaffold ALWAYS pins a clean
// `require github.com/reliant-labs/forge/pkg vX.Y.Z` with NO replace and NO
// vendoring — the release stamp or the latest published tag. A `replace`
// directive for forge/pkg must NEVER appear: maintainers building against
// unpublished forge/pkg bridge with a gitignored go.work, handled outside
// forge.
package templates

import (
	"strings"
	"testing"
)

type goModPkgDepData struct {
	Module          string
	GoVersion       string
	RESTEnabled     bool
	ForgePkgVersion string
}

func renderGoMod(t *testing.T, data goModPkgDepData) string {
	t.Helper()
	out, err := ProjectTemplates().Render("go.mod.tmpl", data)
	if err != nil {
		t.Fatalf("render go.mod.tmpl: %v", err)
	}
	return string(out)
}

func TestGoModTemplate_ForgePkgCleanPin(t *testing.T) {
	got := renderGoMod(t, goModPkgDepData{
		Module: "github.com/example/demo", GoVersion: "1.26",
		ForgePkgVersion: "v0.3.0",
	})
	if !strings.Contains(got, "github.com/reliant-labs/forge/pkg v0.3.0") {
		t.Errorf("missing pinned require, got:\n%s", got)
	}
	if strings.Contains(got, "replace github.com/reliant-labs/forge/pkg") {
		t.Errorf("must not emit a forge/pkg replace, got:\n%s", got)
	}
	// The project-local gen replace must survive untouched.
	if !strings.Contains(got, "replace github.com/example/demo/gen => ./gen") {
		t.Errorf("lost the ./gen replace, got:\n%s", got)
	}
}

// A default-published pin (dev builds fall back to the latest tag) is still a
// clean require with no replace.
func TestGoModTemplate_ForgePkgDefaultPin(t *testing.T) {
	got := renderGoMod(t, goModPkgDepData{
		Module: "github.com/example/demo", GoVersion: "1.26",
		ForgePkgVersion: "v0.0.3",
	})
	if !strings.Contains(got, "github.com/reliant-labs/forge/pkg v0.0.3") {
		t.Errorf("missing pinned require, got:\n%s", got)
	}
	if strings.Contains(got, "replace github.com/reliant-labs/forge/pkg") {
		t.Errorf("must not emit a forge/pkg replace, got:\n%s", got)
	}
	if strings.Contains(got, "forge/pkg v0.0.0") {
		t.Errorf("must never pin forge/pkg to the unresolvable v0.0.0 placeholder, got:\n%s", got)
	}
}

// Empty version (a project kind that doesn't depend on forge/pkg): the
// require line is simply omitted; `go mod tidy` resolves it if the code
// imports it. Still no replace.
func TestGoModTemplate_ForgePkgAbsent(t *testing.T) {
	got := renderGoMod(t, goModPkgDepData{
		Module: "github.com/example/demo", GoVersion: "1.26",
	})
	if strings.Contains(got, "reliant-labs/forge/pkg") {
		t.Errorf("empty version: go.mod must not mention forge/pkg, got:\n%s", got)
	}
}

// --- gen/go.mod (the separate gen submodule) --------------------------------

type genGoModPkgDepData struct {
	Module          string
	GoVersion       string
	ForgePkgVersion string
}

func renderGenGoMod(t *testing.T, data genGoModPkgDepData) string {
	t.Helper()
	out, err := ProjectTemplates().Render("gen-go.mod.tmpl", data)
	if err != nil {
		t.Fatalf("render gen-go.mod.tmpl: %v", err)
	}
	return string(out)
}

// gen/ mirrors the root pin (same published version), no replace.
func TestGenGoModTemplate_ForgePkgCleanPin(t *testing.T) {
	got := renderGenGoMod(t, genGoModPkgDepData{
		Module: "github.com/example/demo", GoVersion: "1.26",
		ForgePkgVersion: "v0.0.3",
	})
	if !strings.Contains(got, "github.com/reliant-labs/forge/pkg v0.0.3") {
		t.Errorf("gen pin: missing pinned require, got:\n%s", got)
	}
	if strings.Contains(got, "replace github.com/reliant-labs/forge/pkg") {
		t.Errorf("gen pin: must not emit a replace, got:\n%s", got)
	}
}

// No version (root has no forge/pkg require): gen/ omits forge/pkg entirely
// (never emit the unresolvable v0.0.0) so `go mod tidy` resolves it.
func TestGenGoModTemplate_ForgePkgAbsent(t *testing.T) {
	got := renderGenGoMod(t, genGoModPkgDepData{
		Module: "github.com/example/demo", GoVersion: "1.26",
	})
	if strings.Contains(got, "reliant-labs/forge/pkg") {
		t.Errorf("gen absent: must not mention forge/pkg (never emit unresolvable v0.0.0), got:\n%s", got)
	}
}
