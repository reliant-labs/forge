package forgeconv

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLintProtoTree_GoodFixtureClean verifies that a canonical, well-annotated
// proto file produces no findings. Counter-asserts every rule in one go: the
// good fixture exercises an entity with PK / tenant / timestamps:true plus
// a plain Request/Response message that should NOT trigger pk-annotation
// (the rule fires only on entity-annotated messages).
func TestLintProtoTree_GoodFixtureClean(t *testing.T) {
	res, err := LintProtoTree(filepath.Join("testdata", "good"))
	if err != nil {
		t.Fatalf("LintProtoTree: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected 0 findings on good fixture, got %d:\n%s", len(res.Findings), res.FormatText())
	}
	if res.HasErrors() {
		t.Errorf("good fixture should not have errors")
	}
}

// TestOneServicePerFile_FiresOnSecondService verifies the analyzer fires
// once per extra service (the canonical first one is the one we'd keep,
// so the violation points at SecondService).
func TestOneServicePerFile_FiresOnSecondService(t *testing.T) {
	res, err := LintProtoTree(filepath.Join("testdata", "bad_two_services"))
	if err != nil {
		t.Fatalf("LintProtoTree: %v", err)
	}
	got := findingsForRule(res.Findings, "forgeconv-one-service-per-file")
	if len(got) != 1 {
		t.Fatalf("expected 1 finding for one-service-per-file, got %d:\n%s", len(got), res.FormatText())
	}
	if !strings.Contains(got[0].Message, "SecondService") {
		t.Errorf("finding should reference SecondService; got: %s", got[0].Message)
	}
	if got[0].Severity != SeverityError {
		t.Errorf("one-service-per-file should be an error, got %s", got[0].Severity)
	}
	if !res.HasErrors() {
		t.Errorf("two-services fixture should report errors")
	}
}

// TestPKAnnotation_FiresOnUnannotatedID verifies the analyzer fires when
// an entity-annotated message has an `id` field without `pk: true`. The
// finding should point at the field's line, not the message header, and
// include a copy-pasteable remediation snippet.
func TestPKAnnotation_FiresOnUnannotatedID(t *testing.T) {
	res, err := LintProtoTree(filepath.Join("testdata", "bad_missing_pk"))
	if err != nil {
		t.Fatalf("LintProtoTree: %v", err)
	}
	got := findingsForRule(res.Findings, "forgeconv-pk-annotation")
	if len(got) != 1 {
		t.Fatalf("expected 1 finding for pk-annotation, got %d:\n%s", len(got), res.FormatText())
	}
	if !strings.Contains(got[0].Message, "User") || !strings.Contains(got[0].Message, "id") {
		t.Errorf("finding should reference User and id; got: %s", got[0].Message)
	}
	if !strings.Contains(got[0].Remediation, "pk: true") {
		t.Errorf("remediation should include `pk: true`; got: %s", got[0].Remediation)
	}
	if got[0].Severity != SeverityError {
		t.Errorf("pk-annotation should be an error, got %s", got[0].Severity)
	}
}

// TestTimestampsAnnotation_FiresWithoutEntityTimestamps verifies that a
// `created_at` / `updated_at` Timestamp field in an entity that doesn't
// set `timestamps: true` produces a finding per offending field.
func TestTimestampsAnnotation_FiresWithoutEntityTimestamps(t *testing.T) {
	res, err := LintProtoTree(filepath.Join("testdata", "bad_missing_timestamps"))
	if err != nil {
		t.Fatalf("LintProtoTree: %v", err)
	}
	got := findingsForRule(res.Findings, "forgeconv-timestamps")
	// Two timestamp fields → two findings.
	if len(got) != 2 {
		t.Fatalf("expected 2 timestamp findings, got %d:\n%s", len(got), res.FormatText())
	}
	for _, f := range got {
		if !strings.Contains(f.Remediation, "timestamps: true") {
			t.Errorf("remediation should mention `timestamps: true`; got: %s", f.Remediation)
		}
	}
}

// TestTenantAnnotation_WarnsOnTenantShapedFieldWithoutMarker verifies the
// tenant-annotation rule fires only when an entity already has SOME field
// marked tenant: true (so we know the entity is tenant-scoped), and a
// tenant-shaped neighbor is missing the annotation.
func TestTenantAnnotation_WarnsOnTenantShapedFieldWithoutMarker(t *testing.T) {
	res, err := LintProtoTree(filepath.Join("testdata", "bad_missing_tenant"))
	if err != nil {
		t.Fatalf("LintProtoTree: %v", err)
	}
	got := findingsForRule(res.Findings, "forgeconv-tenant-annotation")
	if len(got) != 1 {
		t.Fatalf("expected 1 tenant-annotation finding, got %d:\n%s", len(got), res.FormatText())
	}
	if !strings.Contains(got[0].Message, "org_id") {
		t.Errorf("finding should reference org_id; got: %s", got[0].Message)
	}
	if got[0].Severity != SeverityWarning {
		t.Errorf("tenant-annotation should be a warning, got %s", got[0].Severity)
	}
	// Warnings should NOT cause HasErrors() to flip true.
	if res.HasErrors() {
		t.Errorf("tenant-annotation alone shouldn't gate the build")
	}
}

// TestParseProtoFile_HandlesMultiLineFieldAnnotations is a unit test for
// the parser's ability to span field annotations across multiple lines
// (the canonical scaffold style — `string id = 1 [(forge.v1.field) = {\n  pk: true\n}];`).
// Without this support the pk-annotation rule would false-positive on
// every entity in the wild.
func TestParseProtoFile_HandlesMultiLineFieldAnnotations(t *testing.T) {
	content := `syntax = "proto3";

package x.v1;

import "forge/v1/forge.proto";

message Item {
  option (forge.v1.entity) = {
    table: "items"
    timestamps: true
  };

  string id = 1 [(forge.v1.field) = {
    pk: true
  }];

  string name = 2;
}
`
	pf := parseProtoFile("x.proto", content)
	if len(pf.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(pf.Messages))
	}
	msg := pf.Messages[0]
	if !msg.HasEntityAnnotation {
		t.Errorf("Item should have entity annotation")
	}
	if !msg.HasTimestampsTrue {
		t.Errorf("Item should have timestamps: true")
	}
	if msg.PKField != "id" {
		t.Errorf("expected PKField=id, got %q", msg.PKField)
	}
	if len(msg.Fields) != 2 {
		t.Errorf("expected 2 fields, got %d", len(msg.Fields))
	}
}

// TestParseProtoFile_IgnoresVendoredForgeProto verifies that proto files
// inside `proto/forge/` are skipped — those are the vendored annotation
// schemas, not user code, and they declare a (different) FieldOptions
// extension that our regex shouldn't touch.
func TestLintProtoTree_SkipsVendoredForgeAnnotations(t *testing.T) {
	tmp := t.TempDir()
	// Write a proto under proto/forge/v1/ that, if scanned, would
	// definitely trigger findings (multiple "service" tokens, etc.).
	// LintProtoTree must skip it.
	dir := filepath.Join(tmp, "proto", "forge", "v1")
	if err := mkdirAll(dir); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	must(t, writeFile(filepath.Join(dir, "forge.proto"), `syntax = "proto3";
package forge.v1;
service A {}
service B {}
message Thing { string id = 1; }
`))
	res, err := LintProtoTree(tmp)
	if err != nil {
		t.Fatalf("LintProtoTree: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected 0 findings (vendored skipped), got %d:\n%s",
			len(res.Findings), res.FormatText())
	}
}

// TestEntityWithoutAnnotation_IsNotEntity verifies that a plain message
// with `string id = 1;` and no entity annotation is not treated as an
// entity — this is the legacy auto-detect path that we explicitly removed.
func TestEntityWithoutAnnotation_IsNotEntity(t *testing.T) {
	content := `syntax = "proto3";
package x.v1;
message NotAnEntity {
  string id = 1;
  string name = 2;
}
`
	findings := lintProtoFile("x.proto", content, LintOptions{})
	for _, f := range findings {
		if f.Rule == "forgeconv-pk-annotation" {
			t.Errorf("plain message without (forge.v1.entity) must not trigger pk-annotation; got: %s", f.Message)
		}
	}
}

// Internal-package contract-shape, interactor-deps, adapter-no-rpc, and
// utility-skip tests moved to internal/contractcheck/ on 2026-06-04 when
// those four rule files were consolidated under a unified Inspect engine.
// See internal/contractcheck/*_test.go.

// TestMethodAuthAnnotation_FiresOnUnannotatedRPC verifies the analyzer
// flags every RPC that declares no `(forge.v1.method)` annotation
// (auth-by-omission) and leaves annotated RPCs alone. The fixture has
// one annotated RPC (Create) and two unannotated (Get single-line,
// Delete empty-body), so exactly two findings are expected. Default
// severity is warning.
func TestMethodAuthAnnotation_FiresOnUnannotatedRPC(t *testing.T) {
	res, err := LintProtoTree(filepath.Join("testdata", "bad_missing_method_auth"))
	if err != nil {
		t.Fatalf("LintProtoTree: %v", err)
	}
	got := findingsForRule(res.Findings, "forgeconv-method-auth-annotation")
	if len(got) != 2 {
		t.Fatalf("expected 2 method-auth findings, got %d:\n%s", len(got), res.FormatText())
	}
	names := map[string]bool{}
	for _, f := range got {
		if f.Severity != SeverityWarning {
			t.Errorf("default severity should be warning, got %s", f.Severity)
		}
		if !strings.Contains(f.Remediation, "auth_required") {
			t.Errorf("remediation should mention auth_required; got: %s", f.Remediation)
		}
		for _, m := range []string{"Get", "Delete"} {
			if strings.Contains(f.Message, "\""+m+"\"") {
				names[m] = true
			}
		}
	}
	if !names["Get"] || !names["Delete"] {
		t.Errorf("expected findings for Get and Delete; got messages: %v", got)
	}
	// The annotated Create RPC must NOT be flagged.
	for _, f := range got {
		if strings.Contains(f.Message, "\"Create\"") {
			t.Errorf("annotated RPC Create should not be flagged: %s", f.Message)
		}
	}
}

// TestMethodAuthAnnotation_StrictEscalatesToError verifies that strict
// mode escalates the auth-by-omission finding from warning to error so
// `forge lint --strict` (and CI) fail on it.
func TestMethodAuthAnnotation_StrictEscalatesToError(t *testing.T) {
	res, err := LintProtoTreeOpts(filepath.Join("testdata", "bad_missing_method_auth"), LintOptions{Strict: true})
	if err != nil {
		t.Fatalf("LintProtoTreeOpts: %v", err)
	}
	got := findingsForRule(res.Findings, "forgeconv-method-auth-annotation")
	if len(got) != 2 {
		t.Fatalf("expected 2 method-auth findings in strict mode, got %d", len(got))
	}
	for _, f := range got {
		if f.Severity != SeverityError {
			t.Errorf("strict mode should escalate to error, got %s", f.Severity)
		}
	}
	if !res.HasErrors() {
		t.Errorf("strict mode should make the result report errors")
	}
}

// findingsForRule filters a finding slice to a single rule. Keeps tests
// focused — the analyzer pipeline runs every rule in one pass, so a fixture
// could surface unrelated findings.
func findingsForRule(findings []Finding, rule string) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Rule == rule {
			out = append(out, f)
		}
	}
	return out
}

// ─── tiny test helpers (keep test file self-contained) ─────────────

func mkdirAll(dir string) error {
	return mkdirAllImpl(dir)
}

func writeFile(path, content string) error {
	return writeFileImpl(path, content)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}
