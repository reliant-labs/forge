package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
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
		"version", "shape", "environments", "conventions", "codegen",
		"packs", "pack_graph", "proto_migration_alignment",
		"migration_safety", "wire_coverage", "scaffold_markers",
		"crud_stubs", "diagnostics", "deps",
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

// TestAuditEnvironments_WarnsOnMissingCluster confirms the env-cluster
// audit warns when a non-dev environment is declared without cluster:
// (so `forge deploy <env>` can't guard against wrong-context applies).
// Dev gets a safe default (k3d-<project>) so it does NOT warn.
func TestAuditEnvironments_WarnsOnMissingCluster(t *testing.T) {
	cfg := &config.ProjectConfig{
		Name: "cp-forge",
		Envs: []config.EnvironmentConfig{
			{Name: "dev"},     // OK — defaults to k3d-cp-forge
			{Name: "staging"}, // warn — no default
			{Name: "prod", Cluster: "gke_acme-prod"},
		},
	}
	cat := auditEnvironments(cfg)
	if cat.Status != AuditStatusWarn {
		t.Fatalf("status: want warn, got %q (summary=%q)", cat.Status, cat.Summary)
	}
	if !strings.Contains(cat.Summary, "staging") {
		t.Errorf("summary should mention the offending env, got %q", cat.Summary)
	}
	if !strings.Contains(cat.Summary, "1 env(s)") {
		t.Errorf("summary should report the count (only staging), got %q", cat.Summary)
	}
}

// TestAuditEnvironments_AllSet returns ok when every non-dev env
// declares cluster: explicitly.
func TestAuditEnvironments_AllSet(t *testing.T) {
	cfg := &config.ProjectConfig{
		Name: "cp-forge",
		Envs: []config.EnvironmentConfig{
			{Name: "dev"},
			{Name: "staging", Cluster: "gke_acme-staging"},
			{Name: "prod", Cluster: "gke_acme-prod"},
		},
	}
	cat := auditEnvironments(cfg)
	if cat.Status != AuditStatusOK {
		t.Errorf("status: want ok, got %q (summary=%q)", cat.Status, cat.Summary)
	}
}

// TestAuditEnvironments_NoEnvs returns ok (n/a) when forge.yaml has
// no environments declared at all.
func TestAuditEnvironments_NoEnvs(t *testing.T) {
	cfg := &config.ProjectConfig{Name: "cp-forge"}
	cat := auditEnvironments(cfg)
	if cat.Status != AuditStatusOK {
		t.Errorf("status: want ok, got %q", cat.Status)
	}
}

// TestAuditCRUDStubs_NoStubs returns ok when the project has no
// handlers_crud_gen.go files at all (the common case for projects
// whose protos all match AIP-158 conventions — every method gets a
// real CRUD body, no stubs emitted).
func TestAuditCRUDStubs_NoStubs(t *testing.T) {
	dir := t.TempDir()
	cat := auditCRUDStubs(dir)
	if cat.Status != AuditStatusOK {
		t.Errorf("status: want ok, got %q (summary=%q)", cat.Status, cat.Summary)
	}
	if !strings.Contains(cat.Summary, "0 CRUD") {
		t.Errorf("summary should report 0 stubs, got %q", cat.Summary)
	}
	if total, _ := cat.Details["total_stubs"].(int); total != 0 {
		t.Errorf("total_stubs: want 0, got %d", total)
	}
}

// TestAuditCRUDStubs_DetectsStub fixtures a handlers_crud_gen.go
// carrying a FORGE_CRUD_SHAPE_MISMATCH marker and confirms audit
// surfaces (a) warn status, (b) the file path, (c) the method name
// stitched to the marker, and (d) the reason text. This is the
// kalshi-trader friction's regression case — ListSettlements
// returning CodeUnimplemented in production must be a structured
// finding, not a buried comment in a generated file.
func TestAuditCRUDStubs_DetectsStub(t *testing.T) {
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "handlers", "api")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `// Code generated by forge. DO NOT EDIT.
package api

import (
	"context"
	"connectrpc.com/connect"
	pb "example.com/p/gen/services/api/v1"
)

// ListSettlements implements the ListSettlements RPC.
//
// FORGE_CRUD_SHAPE_MISMATCH: request ListSettlementsRequest lacks page_size (AIP-158 pagination assumed by template)
//
// Replace this stub with a hand-written handler in a sibling file.
func (s *Service) ListSettlements(
	ctx context.Context,
	req *connect.Request[pb.ListSettlementsRequest],
) (*connect.Response[pb.ListSettlementsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
`
	if err := os.WriteFile(filepath.Join(pkgDir, "handlers_crud_gen.go"), []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cat := auditCRUDStubs(dir)
	if cat.Status != AuditStatusWarn {
		t.Errorf("status: want warn, got %q (summary=%q)", cat.Status, cat.Summary)
	}
	if total, _ := cat.Details["total_stubs"].(int); total != 1 {
		t.Errorf("total_stubs: want 1, got %v", cat.Details["total_stubs"])
	}
	stubs, ok := cat.Details["stubs"].([]map[string]string)
	if !ok || len(stubs) != 1 {
		t.Fatalf("stubs: want 1 entry, got %#v", cat.Details["stubs"])
	}
	s := stubs[0]
	if s["method"] != "ListSettlements" {
		t.Errorf("method: want ListSettlements, got %q", s["method"])
	}
	if !strings.Contains(s["reason"], "page_size") {
		t.Errorf("reason should carry the marker text, got %q", s["reason"])
	}
	if !strings.HasSuffix(s["file"], "handlers/api/handlers_crud_gen.go") {
		t.Errorf("file: want handlers/api/handlers_crud_gen.go suffix, got %q", s["file"])
	}
}

// TestAuditCRUDStubs_SkipsTemplatesAndTestdata verifies that the audit
// walker's skip set keeps forge's own tree clean — the template
// `handlers_crud_gen.go.tmpl` embeds the literal FORGE_CRUD_SHAPE_MISMATCH
// marker as emission text, and analyzer testdata fixtures may also
// carry it. Either would false-positive the audit if we walked them.
func TestAuditCRUDStubs_SkipsTemplatesAndTestdata(t *testing.T) {
	dir := t.TempDir()
	for _, skipped := range []string{"templates", "testdata"} {
		pkgDir := filepath.Join(dir, skipped, "handlers", "api")
		if err := os.MkdirAll(pkgDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		content := `package api
// FORGE_CRUD_SHAPE_MISMATCH: noisy marker that should be skipped
func (s *Service) Noisy(ctx context.Context, req any) (any, error) { return nil, nil }
`
		if err := os.WriteFile(filepath.Join(pkgDir, "handlers_crud_gen.go"), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	cat := auditCRUDStubs(dir)
	if cat.Status != AuditStatusOK {
		t.Errorf("status: want ok (template + testdata skipped), got %q", cat.Status)
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

// TestAuditDiagnostics_NoFileOK asserts the diagnostics category
// returns ok (with empty list) when pkg/app/diagnostics_gen.go is
// missing — projects that haven't regenerated since hooks 1+2 landed,
// or library/cli projects with no pkg/app/, should not see a warn.
func TestAuditDiagnostics_NoFileOK(t *testing.T) {
	dir := t.TempDir()
	cat := auditDiagnostics(nil, dir)
	if cat.Status != AuditStatusOK {
		t.Errorf("status: want ok, got %q (summary=%q)", cat.Status, cat.Summary)
	}
	// Additive contract: the diagnostics key must always exist, even
	// when empty, so downstream consumers can `.diagnostics[]` without
	// a nil-check.
	if _, ok := cat.Details["diagnostics"]; !ok {
		t.Errorf("details missing diagnostics key (additive-extension contract)")
	}
}

// TestAuditDiagnostics_ParsesRegisterCalls asserts the regex-based
// parser pulls RegisterStub + RegisterNilDep call sites out of an
// emitted diagnostics_gen.go. Verifies both shape (kind + symbol +
// component + dep_name) and the warn-status verdict when entries
// exist.
func TestAuditDiagnostics_ParsesRegisterCalls(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `// Code generated by forge. DO NOT EDIT.
package app

import "github.com/reliant-labs/forge/pkg/diagnostics"

func init() {
	diagnostics.Default.RegisterStub("botconfig.LoadFromYAML", "internal/botconfig/config.go", 18)
	diagnostics.Default.RegisterNilDep("wireWorkerCalibratorRefitDeps", "PgUnsettled", "pkg/app/wire_gen.go", 128)
}
`
	if err := os.WriteFile(filepath.Join(appDir, "diagnostics_gen.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := auditDiagnostics(nil, dir)
	if cat.Status != AuditStatusWarn {
		t.Fatalf("status: want warn, got %q (summary=%q)", cat.Status, cat.Summary)
	}
	if !strings.Contains(cat.Summary, "2 unwired") {
		t.Errorf("summary should mention 2 unwired, got %q", cat.Summary)
	}
	// Round-trip the entries to verify the shape.
	raw, err := json.Marshal(cat.Details["diagnostics"])
	if err != nil {
		t.Fatalf("marshal diagnostics: %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		`"kind":"nil-dep"`,
		`"kind":"stub-impl"`,
		`"symbol":"botconfig.LoadFromYAML"`,
		`"component":"wireWorkerCalibratorRefitDeps"`,
		`"dep_name":"PgUnsettled"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostics JSON missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestAuditDiagnostics_StrictWiringEscalatesToError asserts that with
// `features.strict_wiring: true`, entries flip the status from warn
// to error — CI gates the merge so a strict-wiring project can't ship
// with unresolved entries.
func TestAuditDiagnostics_StrictWiringEscalatesToError(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `package app

import "github.com/reliant-labs/forge/pkg/diagnostics"

func init() {
	diagnostics.Default.RegisterStub("api.Ping", "handlers/api/handlers.go", 12)
}
`
	if err := os.WriteFile(filepath.Join(appDir, "diagnostics_gen.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	strict := true
	cfg := &config.ProjectConfig{
		Features: config.FeaturesConfig{StrictWiring: &strict},
	}
	cat := auditDiagnostics(cfg, dir)
	if cat.Status != AuditStatusError {
		t.Errorf("strict_wiring + 1 entry should be error; got %q (summary=%q)", cat.Status, cat.Summary)
	}
	if got, ok := cat.Details["strict_wiring_enabled"].(bool); !ok || !got {
		t.Errorf("strict_wiring_enabled detail = %v, want true", cat.Details["strict_wiring_enabled"])
	}
}
