package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuditReport_BasicShape exercises buildAuditReport against a
// minimal fixture: a forge.yaml plus a few generated artifacts. We
// assert the JSON shape contains all canonical category keys and that
// rollupStatus produces a valid overall status. We deliberately avoid
// asserting per-category status values — those depend on the project
// state and would make the test brittle to forge convention changes.
func TestAuditReport_BasicShape(t *testing.T) {
	dir := t.TempDir()

	// Minimal forge.yaml so the cfg is loadable.
	yamlBody := `name: test-project
module_path: github.com/test/test-project
version: 0.0.1
forge_version: dev
services: []
environments: []
database: {}
ci: {}
docker: {}
k8s: {}
lint: {}
contracts: {}
auth: {}
docs: {}
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}

	// Empty .forge/checksums.json so the codegen audit has data to read.
	if err := os.MkdirAll(filepath.Join(dir, ".forge"), 0o755); err != nil {
		t.Fatalf("mkdir .forge: %v", err)
	}
	cs := `{"forge_version":"dev","files":{}}`
	if err := os.WriteFile(filepath.Join(dir, ".forge", "checksums.json"), []byte(cs), 0o644); err != nil {
		t.Fatalf("write checksums: %v", err)
	}

	report, err := buildAuditReport(dir)
	if err != nil {
		t.Fatalf("buildAuditReport: %v", err)
	}

	wantKeys := []string{
		"version", "shape", "conventions", "codegen",
		"packs", "pack_graph", "proto_migration_alignment",
		"migration_safety", "wire_coverage", "scaffold_markers", "deps",
	}
	for _, key := range wantKeys {
		if _, ok := report.Categories[key]; !ok {
			t.Errorf("missing audit category: %s", key)
		}
	}

	if report.ProjectName != "test-project" {
		t.Errorf("project name: got %q, want %q", report.ProjectName, "test-project")
	}

	// JSON encoding must round-trip cleanly so sub-agents can consume it.
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded AuditReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Overall status must be one of the canonical strings.
	switch decoded.OverallStatus {
	case AuditStatusOK, AuditStatusWarn, AuditStatusError:
	default:
		t.Errorf("invalid overall status %q", decoded.OverallStatus)
	}
}

// TestAuditReport_NoForgeYaml verifies graceful behavior outside a forge
// project: every category still gets emitted, but version reports an
// error.
func TestAuditReport_NoForgeYaml(t *testing.T) {
	dir := t.TempDir()
	report, err := buildAuditReport(dir)
	if err != nil {
		t.Fatalf("buildAuditReport: %v", err)
	}
	v, ok := report.Categories["version"]
	if !ok {
		t.Fatal("missing version category")
	}
	if v.Status != AuditStatusError {
		t.Errorf("expected error status for missing forge.yaml, got %q", v.Status)
	}
	if !strings.Contains(strings.ToLower(v.Summary), "forge.yaml") {
		t.Errorf("expected summary to mention forge.yaml, got %q", v.Summary)
	}
}
