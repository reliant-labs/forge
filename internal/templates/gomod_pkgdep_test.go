// File: internal/templates/gomod_pkgdep_test.go
//
// Pins the forge/pkg dependency block emitted by go.mod.tmpl in the
// three scaffold modes (see internal/generator/project_pkgdep.go and
// docs/pkg-versioning.md):
//
//   - release: forge binary stamped with a published pkg version →
//     `require github.com/reliant-labs/forge/pkg vX.Y.Z`, NO replace.
//   - dev with sibling forge checkout → require v0.0.0 + host-absolute
//     replace (which `forge generate` later vendors into ./.forge-pkg).
//   - dev without sibling → neither; `go mod tidy` resolves a proxy
//     pseudo-version.
//
// Regression guard: a pinned release require must never be accompanied
// by a replace directive — that is the exact "dev mechanism doing a
// release mechanism's job" coupling this design removes.
package templates

import (
	"strings"
	"testing"
)

type goModPkgDepData struct {
	Module             string
	GoVersion          string
	RESTEnabled        bool
	ForgePkgVersion    string
	ForgePkgDevReplace string
}

func renderGoMod(t *testing.T, data goModPkgDepData) string {
	t.Helper()
	out, err := ProjectTemplates().Render("go.mod.tmpl", data)
	if err != nil {
		t.Fatalf("render go.mod.tmpl: %v", err)
	}
	return string(out)
}

func TestGoModTemplate_ForgePkgReleasePin(t *testing.T) {
	got := renderGoMod(t, goModPkgDepData{
		Module: "github.com/example/demo", GoVersion: "1.26",
		ForgePkgVersion: "v0.3.0",
	})
	if !strings.Contains(got, "github.com/reliant-labs/forge/pkg v0.3.0") {
		t.Errorf("release mode: missing pinned require, got:\n%s", got)
	}
	if strings.Contains(got, "replace github.com/reliant-labs/forge/pkg") {
		t.Errorf("release mode: must not emit a forge/pkg replace, got:\n%s", got)
	}
	// The project-local gen replace must survive untouched in all modes.
	if !strings.Contains(got, "replace github.com/example/demo/gen => ./gen") {
		t.Errorf("release mode: lost the ./gen replace, got:\n%s", got)
	}
}

func TestGoModTemplate_ForgePkgDevSiblingReplace(t *testing.T) {
	got := renderGoMod(t, goModPkgDepData{
		Module: "github.com/example/demo", GoVersion: "1.26",
		ForgePkgDevReplace: "/home/dev/src/forge/pkg",
	})
	if !strings.Contains(got, "github.com/reliant-labs/forge/pkg v0.0.0") {
		t.Errorf("dev mode: missing placeholder require, got:\n%s", got)
	}
	if !strings.Contains(got, "replace github.com/reliant-labs/forge/pkg => /home/dev/src/forge/pkg") {
		t.Errorf("dev mode: missing host-absolute replace, got:\n%s", got)
	}
}

func TestGoModTemplate_ForgePkgDevNoSibling(t *testing.T) {
	got := renderGoMod(t, goModPkgDepData{
		Module: "github.com/example/demo", GoVersion: "1.26",
	})
	if strings.Contains(got, "reliant-labs/forge/pkg") {
		t.Errorf("dev mode without sibling: go.mod must not mention forge/pkg (tidy resolves it), got:\n%s", got)
	}
}

// --- gen/go.mod (the separate gen submodule) --------------------------------

type genGoModPkgDepData struct {
	Module             string
	GoVersion          string
	ForgePkgVersion    string
	ForgePkgGenReplace string
}

func renderGenGoMod(t *testing.T, data genGoModPkgDepData) string {
	t.Helper()
	out, err := ProjectTemplates().Render("gen-go.mod.tmpl", data)
	if err != nil {
		t.Fatalf("render gen-go.mod.tmpl: %v", err)
	}
	return string(out)
}

// Concrete pin (release tag or the pseudo-version this forge binary was
// built against): gen/ pins the same version, no replace.
func TestGenGoModTemplate_ForgePkgConcretePin(t *testing.T) {
	got := renderGenGoMod(t, genGoModPkgDepData{
		Module: "github.com/example/demo", GoVersion: "1.26",
		ForgePkgVersion: "v0.0.0-20260624040937-ce5dfbd929ed",
	})
	if !strings.Contains(got, "github.com/reliant-labs/forge/pkg v0.0.0-20260624040937-ce5dfbd929ed") {
		t.Errorf("gen concrete pin: missing pinned require, got:\n%s", got)
	}
	if strings.Contains(got, "replace github.com/reliant-labs/forge/pkg") {
		t.Errorf("gen concrete pin: must not emit a replace, got:\n%s", got)
	}
}

// Dev-sibling flow: placeholder require + rebased replace.
func TestGenGoModTemplate_ForgePkgDevReplace(t *testing.T) {
	got := renderGenGoMod(t, genGoModPkgDepData{
		Module: "github.com/example/demo", GoVersion: "1.26",
		ForgePkgGenReplace: "../../forge/pkg",
	})
	if !strings.Contains(got, "github.com/reliant-labs/forge/pkg v0.0.0") {
		t.Errorf("gen dev mode: missing placeholder require, got:\n%s", got)
	}
	if !strings.Contains(got, "replace github.com/reliant-labs/forge/pkg => ../../forge/pkg") {
		t.Errorf("gen dev mode: missing rebased replace, got:\n%s", got)
	}
}

// Local go.work build with no sibling and no build-info version: gen/ omits
// forge/pkg entirely (no unresolvable v0.0.0) so `go mod tidy` resolves it —
// this is the regression the old template hard-coding `v0.0.0` broke.
func TestGenGoModTemplate_ForgePkgNoVersionNoReplace(t *testing.T) {
	got := renderGenGoMod(t, genGoModPkgDepData{
		Module: "github.com/example/demo", GoVersion: "1.26",
	})
	if strings.Contains(got, "reliant-labs/forge/pkg") {
		t.Errorf("gen no-version no-replace: must not mention forge/pkg (never emit unresolvable v0.0.0), got:\n%s", got)
	}
}

// TestGoModTemplate_ModesAreMutuallyExclusive documents that a caller
// bug supplying BOTH fields resolves in favor of the release pin — the
// template's {{if}} chain checks ForgePkgVersion first — so a stamped
// release binary can never leak a dev replace into a scaffold.
func TestGoModTemplate_ModesAreMutuallyExclusive(t *testing.T) {
	got := renderGoMod(t, goModPkgDepData{
		Module: "github.com/example/demo", GoVersion: "1.26",
		ForgePkgVersion: "v0.3.0", ForgePkgDevReplace: "/home/dev/src/forge/pkg",
	})
	if !strings.Contains(got, "github.com/reliant-labs/forge/pkg v0.3.0") {
		t.Errorf("expected release pin to win, got:\n%s", got)
	}
	if strings.Contains(got, "replace github.com/reliant-labs/forge/pkg") {
		t.Errorf("release pin present: dev replace must be suppressed, got:\n%s", got)
	}
}
