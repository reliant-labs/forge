package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

// TestGenerateFrontendPages_PreservesHandEditsOnRegenerate is the
// regression test for the kalshi-trader friction round's blocker:
// `forge generate` unconditionally re-rendered
// `frontends/<fe>/src/app/<slug>/page.tsx` despite every page template
// carrying a `// forge:scaffold one-shot — list page emitted by 'forge
// add page'` banner promising the user that hand-edits would survive.
// Pre-fix, the renderer used a bare `os.WriteFile` with no
// stat-check guard, so the second `forge generate` invocation
// silently clobbered the user's edits.
//
// Fix: emit through `checksums.WriteGeneratedFileTier2` with a
// stat-check pre-guard mirroring `emitTier2OnceIfMissing`, so the
// destination is left alone on re-run unless --force.
func TestGenerateFrontendPages_PreservesHandEditsOnRegenerate(t *testing.T) {
	projectDir := t.TempDir()

	cfg := &config.ProjectConfig{
		Name: "demo",
		Frontends: []config.FrontendConfig{
			{Name: "dashboard", Type: "nextjs"},
		},
	}
	services := []codegen.ServiceDef{
		{
			Name:    "ClinicService",
			Package: "demo.v1",
			Methods: []codegen.Method{
				{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
			},
		},
	}
	entities := []codegen.EntityDef{{Name: "Patient"}}
	cs := &checksums.FileChecksums{Files: make(map[string]checksums.FileChecksumEntry)}

	// First run: scaffolds the file.
	if err := generateFrontendPages(cfg, services, projectDir, entities, cs, false); err != nil {
		t.Fatalf("first generateFrontendPages: %v", err)
	}

	pagePath := filepath.Join(projectDir, "frontends", "dashboard", "src", "app", "patients", "page.tsx")
	if _, err := os.Stat(pagePath); err != nil {
		t.Fatalf("expected scaffolded page at %s after first run, got: %v", pagePath, err)
	}

	// Simulate a hand-edit on the scaffolded page.
	const handEdited = `// User hand-edited this page; the next forge generate must not clobber it.
export default function PatientsPage() { return null; }
`
	if err := os.WriteFile(pagePath, []byte(handEdited), 0o644); err != nil {
		t.Fatalf("simulating hand-edit: %v", err)
	}

	// Second run: hand-edit must survive.
	if err := generateFrontendPages(cfg, services, projectDir, entities, cs, false); err != nil {
		t.Fatalf("second generateFrontendPages: %v", err)
	}

	got, err := os.ReadFile(pagePath)
	if err != nil {
		t.Fatalf("read page after re-run: %v", err)
	}
	if string(got) != handEdited {
		t.Errorf("hand-edited page was clobbered by re-run of generateFrontendPages.\nwant:\n%s\ngot:\n%s", handEdited, string(got))
	}
}

// TestGenerateFrontendPages_ForceReScaffoldsExistingPage asserts the
// `--force` opt-out (forge generate --force) still re-emits the page
// from the template. This is the documented escape hatch for users
// who want to throw away local edits and start fresh from the
// scaffold — the Tier-2 lifecycle's only safe override.
func TestGenerateFrontendPages_ForceReScaffoldsExistingPage(t *testing.T) {
	projectDir := t.TempDir()

	cfg := &config.ProjectConfig{
		Name: "demo",
		Frontends: []config.FrontendConfig{
			{Name: "dashboard", Type: "nextjs"},
		},
	}
	services := []codegen.ServiceDef{
		{
			Name:    "ClinicService",
			Package: "demo.v1",
			Methods: []codegen.Method{
				{Name: "ListPatients", InputType: "ListPatientsRequest", OutputType: "ListPatientsResponse"},
			},
		},
	}
	entities := []codegen.EntityDef{{Name: "Patient"}}
	cs := &checksums.FileChecksums{Files: make(map[string]checksums.FileChecksumEntry)}

	if err := generateFrontendPages(cfg, services, projectDir, entities, cs, false); err != nil {
		t.Fatalf("first generateFrontendPages: %v", err)
	}

	pagePath := filepath.Join(projectDir, "frontends", "dashboard", "src", "app", "patients", "page.tsx")
	const handEdited = `// user edit that --force should clobber`
	if err := os.WriteFile(pagePath, []byte(handEdited), 0o644); err != nil {
		t.Fatalf("simulating hand-edit: %v", err)
	}

	if err := generateFrontendPages(cfg, services, projectDir, entities, cs, true); err != nil {
		t.Fatalf("force generateFrontendPages: %v", err)
	}

	got, err := os.ReadFile(pagePath)
	if err != nil {
		t.Fatalf("read page after force re-run: %v", err)
	}
	// After --force, the file should be back to the scaffold template (no
	// longer carrying the hand-edit marker) and should at minimum carry
	// the canonical `"use client"` directive every list-page template ships.
	if strings.Contains(string(got), handEdited) {
		t.Errorf("--force did NOT re-scaffold the page; hand-edit survived. Got:\n%s", string(got))
	}
	if !strings.Contains(string(got), `"use client"`) {
		t.Errorf("--force-rendered page does not look like the scaffold template (missing `\"use client\"`). Got:\n%s", string(got))
	}
}
