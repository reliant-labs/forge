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
// carrying a canonical Tier-2 "yours:" banner (list page emitted by 'forge
// add page'` banner promising the user that hand-edits would survive.
// Pre-fix, the renderer used a bare `os.WriteFile` with no
// stat-check guard, so the second `forge generate` invocation
// silently clobbered the user's edits.
//
// Fix: emit through `checksums.WriteGeneratedFileTier2` with a
// stat-check pre-guard mirroring `emitTier2OnceIfMissing`, so the
// destination is left alone on re-run unless the `--reset-tier2`
// hook is installed.
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
	if err := generateFrontendPages(cfg, services, projectDir, entities, cs); err != nil {
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
	if err := generateFrontendPages(cfg, services, projectDir, entities, cs); err != nil {
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

// (The old TestGenerateFrontendPages_ForcePreservesHandEdits is gone:
// `force` is no longer plumbed into the frontend page renderer at all —
// `--force` is scoped to the Tier-1 files the stomp guard flagged
// (generate_force_scope_test.go), so there is no force path that can
// reach a Tier-2 page. The chokepoint-level pin lives in
// internal/checksums/tier2_test.go.)

// TestGenerateFrontendPages_ResetTier2OverwritesHandEdits asserts the
// new `--reset-tier2` opt-in path: when the pipeline installs a Tier-2
// overwrite hook returning true (the shape `--reset-tier2 --yes`
// produces), hand-edits ARE overwritten. This is the escape hatch the
// item-15 contract preserves.
func TestGenerateFrontendPages_ResetTier2OverwritesHandEdits(t *testing.T) {
	checksums.ResetTier2State()
	defer checksums.ResetTier2State()

	projectDir := t.TempDir()
	cfg := &config.ProjectConfig{
		Name:      "demo",
		Frontends: []config.FrontendConfig{{Name: "dashboard", Type: "nextjs"}},
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

	if err := generateFrontendPages(cfg, services, projectDir, entities, cs); err != nil {
		t.Fatalf("first generateFrontendPages: %v", err)
	}

	pagePath := filepath.Join(projectDir, "frontends", "dashboard", "src", "app", "patients", "page.tsx")
	const handEdited = `// user edit that --reset-tier2 --yes SHOULD clobber`
	if err := os.WriteFile(pagePath, []byte(handEdited), 0o644); err != nil {
		t.Fatalf("simulating hand-edit: %v", err)
	}

	// --reset-tier2 --yes shape: unconditional approval.
	checksums.Tier2OverwriteFn = func(string) bool { return true }
	if err := generateFrontendPages(cfg, services, projectDir, entities, cs); err != nil {
		t.Fatalf("reset-tier2 generateFrontendPages: %v", err)
	}

	got, err := os.ReadFile(pagePath)
	if err != nil {
		t.Fatalf("read page after reset-tier2 re-run: %v", err)
	}
	if strings.Contains(string(got), handEdited) {
		t.Errorf("--reset-tier2 --yes did NOT re-scaffold the page; hand-edit survived. Got:\n%s", string(got))
	}
	if !strings.Contains(string(got), `"use client"`) {
		t.Errorf("--reset-tier2-rendered page does not look like the scaffold template (missing `\"use client\"`). Got:\n%s", string(got))
	}
}
