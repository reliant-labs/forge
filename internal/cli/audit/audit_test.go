package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/cli/audittype"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// TestAuditVersion_RealComparison pins fr-82b717f521: audit must do a
// REAL pinned-vs-binary comparison and never print a false-green
// "matches" when the strings differ — even when the binary is a dev /
// pseudo-version build (the dogfooding case where pins drift).
func TestAuditVersion_RealComparison(t *testing.T) {
	t.Cleanup(func() { buildinfo.Set("dev", "unknown", "unknown") })

	t.Run("stale pin is flagged, not falsely green", func(t *testing.T) {
		buildinfo.Set("v0.0.0-20260612070344-a3e3b883c97c", "unknown", "unknown")
		cfg := &config.ProjectConfig{ForgeVersion: "v0.0.0-20260530233501-ec0254f463b3"}
		cat := auditVersion(cfg, t.TempDir())
		if cat.Status != audittype.StatusWarn {
			t.Errorf("stale pin must WARN, got %q (summary=%q)", cat.Status, cat.Summary)
		}
		if strings.Contains(cat.Summary, "matches binary") {
			t.Errorf("must not claim 'matches binary' on a divergent pin: %q", cat.Summary)
		}
		if !strings.Contains(cat.Summary, "does NOT match") {
			t.Errorf("summary should report the mismatch: %q", cat.Summary)
		}
	})

	t.Run("exact match is OK", func(t *testing.T) {
		buildinfo.Set("v0.0.0-20260612070344-a3e3b883c97c", "unknown", "unknown")
		cfg := &config.ProjectConfig{ForgeVersion: "v0.0.0-20260612070344-a3e3b883c97c"}
		cat := auditVersion(cfg, t.TempDir())
		if cat.Status != audittype.StatusOK {
			t.Errorf("exact match must be OK, got %q (summary=%q)", cat.Status, cat.Summary)
		}
		if !strings.Contains(cat.Summary, "matches binary") {
			t.Errorf("summary should confirm the match: %q", cat.Summary)
		}
	})

	t.Run("missing pin warns", func(t *testing.T) {
		buildinfo.Set("v1.0.0", "unknown", "unknown")
		cfg := &config.ProjectConfig{ForgeVersion: ""}
		cat := auditVersion(cfg, t.TempDir())
		if cat.Status != audittype.StatusWarn {
			t.Errorf("missing pin must WARN, got %q", cat.Status)
		}
	})

	t.Run("deliberate 0.0.0 sentinel is OK, not a stale-pin warn", func(t *testing.T) {
		// forge-as-its-own-first-user pins 0.0.0 (see forge.yaml); it is
		// intentionally never equal to the pseudo-version binary and must
		// not warn.
		buildinfo.Set("v0.0.0-20260612070344-a3e3b883c97c", "unknown", "unknown")
		cfg := &config.ProjectConfig{ForgeVersion: "0.0.0"}
		cat := auditVersion(cfg, t.TempDir())
		if cat.Status != audittype.StatusOK {
			t.Errorf("0.0.0 sentinel must be OK, got %q (summary=%q)", cat.Status, cat.Summary)
		}
		if strings.Contains(cat.Summary, "does NOT match") {
			t.Errorf("0.0.0 sentinel must not be reported as a mismatch: %q", cat.Summary)
		}
		if !strings.Contains(cat.Summary, "0.0.0") {
			t.Errorf("summary should explain the 0.0.0 sentinel: %q", cat.Summary)
		}
	})
}

// TestAuditVersion_DivergentPins pins the second half of fr-82b717f521:
// when forge.yaml, the CI workflow, and .forge state carry three
// different forge versions, audit flags the divergence (nothing else
// does).
func TestAuditVersion_DivergentPins(t *testing.T) {
	t.Cleanup(func() { buildinfo.Set("dev", "unknown", "unknown") })
	buildinfo.Set("v0.0.0-20260612070344-a3e3b883c97c", "unknown", "unknown")

	dir := t.TempDir()
	// CI workflow pins a third, different version.
	wfDir := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ci := "jobs:\n  verify:\n    steps:\n      - run: go install github.com/reliant-labs/forge/cmd/forge@v0.0.0-20260611225538-09863e5d16f4\n"
	if err := os.WriteFile(filepath.Join(wfDir, "ci.yml"), []byte(ci), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.ProjectConfig{ForgeVersion: "v0.0.0-20260530233501-ec0254f463b3"}
	cat := auditVersion(cfg, dir)
	if cat.Status != audittype.StatusWarn {
		t.Fatalf("divergent pins must WARN, got %q (summary=%q)", cat.Status, cat.Summary)
	}
	if !strings.Contains(cat.Summary, "divergent forge version pins") {
		t.Errorf("summary should flag divergent pins: %q", cat.Summary)
	}
	if got, ok := cat.Details["ci_pin"].(string); !ok || got != "v0.0.0-20260611225538-09863e5d16f4" {
		t.Errorf("ci_pin detail = %v, want the CI workflow's ref", cat.Details["ci_pin"])
	}
}

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
	// Empty components.json → service kind (the empty-service shell).
	writeComponentsJSONTest(t, dir)

	// No .forge state files at all — the steady state in the
	// self-certifying era (the manifest-era empty checksums.json would
	// now read as a pending legacy migration). The codegen audit reads
	// ownership from the files themselves.

	report, err := buildAuditReport(testFactory(auditAPIConfig{}), dir)
	if err != nil {
		t.Fatalf("buildAuditReport: %v", err)
	}

	wantKeys := []string{
		"version", "shape", "features", "environments", "external_builds",
		"conventions", "codegen",
		"packs", "pack_graph",
		"migration_safety", "wire_coverage", "scaffold_markers",
		"crud_stubs", "diagnostics", "deps", "friction",
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
	var decoded Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Overall status must be one of the canonical strings.
	switch decoded.OverallStatus {
	case audittype.StatusOK, audittype.StatusWarn, audittype.StatusError:
	default:
		t.Errorf("invalid overall status %q", decoded.OverallStatus)
	}
}

// TestAuditEnvironments_ListsFilesystem confirms the audit walks
// deploy/kcl/<env>/main.k to enumerate envs and emits one entry per
// declared env.
func TestAuditEnvironments_ListsFilesystem(t *testing.T) {
	f := testFactory(auditAPIConfig{listEnvs: []string{"dev", "prod"}})
	cat := auditEnvironments(f, t.TempDir())
	if cat.Status != audittype.StatusOK {
		t.Errorf("status: want ok, got %q (summary=%q)", cat.Status, cat.Summary)
	}
	if !strings.Contains(cat.Summary, "2 environment(s)") {
		t.Errorf("summary should report 2 envs, got %q", cat.Summary)
	}
}

// TestAuditEnvironments_NoEnvs returns ok (n/a) when no deploy/kcl/<env>
// directories are present.
func TestAuditEnvironments_NoEnvs(t *testing.T) {
	cat := auditEnvironments(testFactory(auditAPIConfig{}), t.TempDir())
	if cat.Status != audittype.StatusOK {
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
	if cat.Status != audittype.StatusOK {
		t.Errorf("status: want ok, got %q (summary=%q)", cat.Status, cat.Summary)
	}
	if !strings.Contains(cat.Summary, "0 custom-read-shape CRUD stubs") {
		t.Errorf("summary should report 0 stubs, got %q", cat.Summary)
	}
	if total, _ := cat.Details["total_stubs"].(int); total != 0 {
		t.Errorf("total_stubs: want 0, got %d", total)
	}
}

// TestAuditCRUDStubs_DetectsLegacyMarker fixtures a handlers_crud_gen.go
// carrying the PRE-RENAME FORGE_CRUD_SHAPE_MISMATCH marker and confirms
// audit still surfaces it — the marker was renamed to
// forge:custom-read-shape, and the old spelling stays recognized for one
// release so existing files keep producing findings. This is also the
// kalshi-trader friction's regression case — ListSettlements
// returning CodeUnimplemented in production must be a structured
// finding, not a buried comment in a generated file.
func TestAuditCRUDStubs_DetectsLegacyMarker(t *testing.T) {
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "internal", "handlers", "api")
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
	if cat.Status != audittype.StatusWarn {
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

// TestAuditCRUDStubs_DetectsCurrentMarker covers the renamed marker the
// shim template emits today (`forge:custom-read-shape`) in the
// user-owned handlers_crud.go, and the grep-compat details the finding
// carries for consumers migrating off the old string.
func TestAuditCRUDStubs_DetectsCurrentMarker(t *testing.T) {
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "internal", "handlers", "api")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `package api

import (
	"context"
	"connectrpc.com/connect"
	pb "example.com/p/gen/services/api/v1"
)

// ListTrades implements the ListTrades RPC.
//
// forge:custom-read-shape: request ListTradesRequest shaped by ticker+limit (observed fields: ticker, limit)
//
// Custom read shape — yours to implement.
func (s *Service) ListTrades(
	ctx context.Context,
	req *connect.Request[pb.ListTradesRequest],
) (*connect.Response[pb.ListTradesResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
`
	if err := os.WriteFile(filepath.Join(pkgDir, "handlers_crud.go"), []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cat := auditCRUDStubs(dir)
	if cat.Status != audittype.StatusWarn {
		t.Errorf("status: want warn, got %q (summary=%q)", cat.Status, cat.Summary)
	}
	stubs, ok := cat.Details["stubs"].([]map[string]string)
	if !ok || len(stubs) != 1 {
		t.Fatalf("stubs: want 1 entry, got %#v", cat.Details["stubs"])
	}
	if stubs[0]["method"] != "ListTrades" {
		t.Errorf("method: want ListTrades, got %q", stubs[0]["method"])
	}
	if !strings.Contains(stubs[0]["reason"], "ticker+limit") {
		t.Errorf("reason should carry the marker text, got %q", stubs[0]["reason"])
	}
	// Grep-compat note: consumers must be able to discover the rename
	// from the finding itself.
	if got, _ := cat.Details["marker"].(string); got != "forge:custom-read-shape" {
		t.Errorf("marker detail: want forge:custom-read-shape, got %q", got)
	}
	if got, _ := cat.Details["legacy_marker"].(string); got != "FORGE_CRUD_SHAPE_MISMATCH" {
		t.Errorf("legacy_marker detail: want FORGE_CRUD_SHAPE_MISMATCH, got %q", got)
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
	if cat.Status != audittype.StatusOK {
		t.Errorf("status: want ok (template + testdata skipped), got %q", cat.Status)
	}
}

// TestAuditReport_NoForgeYaml verifies graceful behavior outside a forge
// project: every category still gets emitted, but version reports an
// error.
func TestAuditReport_NoForgeYaml(t *testing.T) {
	dir := t.TempDir()
	report, err := buildAuditReport(testFactory(auditAPIConfig{}), dir)
	if err != nil {
		t.Fatalf("buildAuditReport: %v", err)
	}
	v, ok := report.Categories["version"]
	if !ok {
		t.Fatal("missing version category")
	}
	if v.Status != audittype.StatusError {
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
	if cat.Status != audittype.StatusOK {
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
	if cat.Status != audittype.StatusWarn {
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
	cfg := &config.ProjectConfig{
		Features: config.FeaturesConfig{
			Experimental: config.ExperimentalConfig{StrictWiring: true},
		},
	}
	cat := auditDiagnostics(cfg, dir)
	if cat.Status != audittype.StatusError {
		t.Errorf("strict_wiring + 1 entry should be error; got %q (summary=%q)", cat.Status, cat.Summary)
	}
	if got, ok := cat.Details["strict_wiring_enabled"].(bool); !ok || !got {
		t.Errorf("strict_wiring_enabled detail = %v, want true", cat.Details["strict_wiring_enabled"])
	}
}

// TestAuditFeatures_ZeroConfig asserts the features audit category
// surfaces every feature as enabled when no `features:` block is set
// — the backwards-compat default. Pins the additive-extension shape:
// `.details.resolved.<name>` exists for each Feature* constant.
//
// Also pins the empty-disabled-list guarantee: when no feature is
// off, `details.disabled` is an empty []string not nil, so the JSON
// encoder emits `[]` and `jq '.disabled | length'` returns 0 rather
// than failing on a null.
func TestAuditFeatures_ZeroConfig(t *testing.T) {
	cat := auditFeatures(&config.ProjectConfig{Name: "t"})
	if cat.Status != audittype.StatusOK {
		t.Errorf("status = %q, want ok", cat.Status)
	}
	resolved, ok := cat.Details["resolved"].(map[string]bool)
	if !ok {
		t.Fatalf("details.resolved missing or wrong type: %T", cat.Details["resolved"])
	}
	// Stable features default ON. Experimental features default OFF
	// and are asserted separately below.
	for _, name := range []string{
		config.FeatureBuild, config.FeatureFrontend,
		config.FeaturePacks, config.FeatureCI,
		config.FeatureDocs, config.FeatureObservability,
	} {
		if !resolved[name] {
			t.Errorf("resolved[%q] = false, want true (no features block → stable enabled)", name)
		}
	}
	for _, name := range config.ExperimentalFeatureNames {
		if resolved[name] {
			t.Errorf("resolved[%q] = true, want false (no features block → experimental disabled)", name)
		}
	}
	disabled, ok := cat.Details["disabled"].([]string)
	if !ok {
		t.Fatalf("details.disabled wrong type: %T", cat.Details["disabled"])
	}
	if disabled == nil {
		t.Error("details.disabled is nil — JSON encoding would emit null instead of []")
	}
	if len(disabled) != 0 {
		t.Errorf("details.disabled = %v, want empty", disabled)
	}
}

// TestAuditFeatures_PartialDisable asserts the enabled/disabled
// splits in the details payload are derived from the resolved map:
// disabling a couple of stable features must surface them in
// `disabled` (alphabetised) and remove them from `enabled`.
// Experimental features land in their own buckets so they don't
// pollute the stable-disabled signal.
func TestAuditFeatures_PartialDisable(t *testing.T) {
	off := false
	cfg := &config.ProjectConfig{
		Features: config.FeaturesConfig{Build: &off, Packs: &off},
	}
	cat := auditFeatures(cfg)
	disabled, ok := cat.Details["disabled"].([]string)
	if !ok {
		t.Fatalf("details.disabled wrong type: %T", cat.Details["disabled"])
	}
	wantDisabled := map[string]bool{config.FeatureBuild: true, config.FeaturePacks: true}
	for _, name := range disabled {
		if !wantDisabled[name] {
			t.Errorf("unexpected disabled feature %q", name)
		}
	}
	if len(disabled) != len(wantDisabled) {
		t.Errorf("disabled count = %d, want %d (%v)", len(disabled), len(wantDisabled), disabled)
	}
}

// TestAuditFeatures_ExperimentalBuckets asserts experimental
// features are surfaced in their own buckets and don't pollute
// the stable enabled/disabled lists. A project with no opt-ins
// must show every experimental name in `experimental_available`
// and an empty `experimental_enabled`.
func TestAuditFeatures_ExperimentalBuckets(t *testing.T) {
	cfg := &config.ProjectConfig{
		Features: config.FeaturesConfig{
			Experimental: config.ExperimentalConfig{
				Ingress: true,
			},
		},
	}
	cat := auditFeatures(cfg)
	expEnabled, ok := cat.Details["experimental_enabled"].([]string)
	if !ok {
		t.Fatalf("details.experimental_enabled wrong type: %T", cat.Details["experimental_enabled"])
	}
	wantEnabled := map[string]bool{config.FeatureIngress: true}
	if len(expEnabled) != len(wantEnabled) {
		t.Errorf("experimental_enabled = %v, want %v", expEnabled, wantEnabled)
	}
	for _, name := range expEnabled {
		if !wantEnabled[name] {
			t.Errorf("unexpected experimental_enabled feature %q", name)
		}
	}
	expAvail, ok := cat.Details["experimental_available"].([]string)
	if !ok {
		t.Fatalf("details.experimental_available wrong type: %T", cat.Details["experimental_available"])
	}
	if len(expAvail) != len(config.ExperimentalFeatureNames) {
		t.Errorf("experimental_available count = %d, want %d", len(expAvail), len(config.ExperimentalFeatureNames))
	}
	// Experimental opt-ins must NOT appear in the stable enabled bucket.
	stableEnabled, _ := cat.Details["enabled"].([]string)
	for _, name := range stableEnabled {
		if config.IsExperimentalFeature(name) {
			t.Errorf("experimental feature %q leaked into stable `enabled` bucket", name)
		}
	}
}

// TestAuditFeatures_NilConfig surfaces "no forge.yaml" as error so
// sub-agents branching on `.features.status == "error"` don't get
// a false-ok on a non-forge project.
func TestAuditFeatures_NilConfig(t *testing.T) {
	cat := auditFeatures(nil)
	if cat.Status != audittype.StatusError {
		t.Errorf("nil cfg status = %q, want error", cat.Status)
	}
}

// TestAuditShape_PerRPCStreamingAndMCPCallable pins the additive
// per-RPC extension to the shape category: each service entry gains an
// `rpcs` list with name / streaming / mcp_callable so agents learn —
// before ever talking to forge-mcp — which RPCs the MCP bridge can
// dispatch (unary) and which it cannot (streaming; excluded from MCP
// tools/list). Additive-extension contract: pre-existing keys
// (name/type/rpc_count) are asserted untouched alongside.
func TestAuditShape_PerRPCStreamingAndMCPCallable(t *testing.T) {
	dir := t.TempDir()

	yamlBody := `name: test-project
module_path: github.com/test/test-project
forge_version: dev
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	writeComponentsJSONTest(t, dir, config.ComponentConfig{Name: "tasks", Kind: "server", Path: "internal/tasks"})
	// proto/services must exist for auditShape to attempt the parse.
	if err := os.MkdirAll(filepath.Join(dir, "proto", "services"), 0o755); err != nil {
		t.Fatalf("mkdir proto/services: %v", err)
	}
	// ParseServicesFromProtos reads gen/forge_descriptor.json.
	if err := os.MkdirAll(filepath.Join(dir, "gen"), 0o755); err != nil {
		t.Fatalf("mkdir gen: %v", err)
	}
	descriptor := `{
  "services": [
    {
      "Name": "TasksService",
      "Package": "tasks.v1",
      "Methods": [
        {"Name": "Create", "InputType": "CreateRequest", "OutputType": "CreateResponse"},
        {"Name": "Tail", "InputType": "TailRequest", "OutputType": "TailResponse", "ServerStreaming": true},
        {"Name": "Sync", "InputType": "SyncRequest", "OutputType": "SyncResponse", "ClientStreaming": true, "ServerStreaming": true}
      ]
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "gen", "forge_descriptor.json"), []byte(descriptor), 0o644); err != nil {
		t.Fatalf("write descriptor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/test/test-project\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	cfg, err := generator.ReadProjectConfig(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	// The "tasks" service is a Connect service and is registered (served).
	f := testFactory(auditAPIConfig{
		projectDefinesConnectServices: true,
		isConnectService:              func(config.ComponentConfig) bool { return true },
		registry:                      stubRegistry{exists: true, registered: map[string]bool{"tasks": true}},
	})
	cat := auditShape(f, cfg, dir)

	// Round-trip through JSON — that's the shape sub-agents consume.
	data, err := json.Marshal(cat.Details)
	if err != nil {
		t.Fatalf("marshal details: %v", err)
	}
	var details struct {
		Services []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			RPCCount int    `json:"rpc_count"`
			RPCs     []struct {
				Name        string `json:"name"`
				Streaming   string `json:"streaming"`
				MCPCallable bool   `json:"mcp_callable"`
			} `json:"rpcs"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &details); err != nil {
		t.Fatalf("unmarshal details: %v", err)
	}
	if len(details.Services) != 1 {
		t.Fatalf("services = %#v, want 1 entry", details.Services)
	}
	svc := details.Services[0]
	// Pre-existing keys keep their meaning (additive contract).
	if svc.Name != "tasks" || svc.RPCCount != 3 {
		t.Errorf("name/rpc_count = %q/%d, want tasks/3", svc.Name, svc.RPCCount)
	}
	if len(svc.RPCs) != 3 {
		t.Fatalf("rpcs = %#v, want 3 entries", svc.RPCs)
	}
	want := []struct {
		name      string
		streaming string
		callable  bool
	}{
		{"Create", "", true},
		{"Tail", "server", false},
		{"Sync", "bidi", false},
	}
	for i, w := range want {
		got := svc.RPCs[i]
		if got.Name != w.name || got.Streaming != w.streaming || got.MCPCallable != w.callable {
			t.Errorf("rpcs[%d] = %+v, want %+v", i, got, w)
		}
	}

	// Wire-shape check: unary RPCs must omit the streaming key
	// entirely (omitempty), mirroring the MCP manifest convention.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	firstRPC := raw["services"].([]any)[0].(map[string]any)["rpcs"].([]any)[0].(map[string]any)
	if _, present := firstRPC["streaming"]; present {
		t.Errorf("unary rpc entry must omit streaming key, got %v", firstRPC["streaming"])
	}
}
