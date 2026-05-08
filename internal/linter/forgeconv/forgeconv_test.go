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
	findings := lintProtoFile("x.proto", content)
	for _, f := range findings {
		if f.Rule == "forgeconv-pk-annotation" {
			t.Errorf("plain message without (forge.v1.entity) must not trigger pk-annotation; got: %s", f.Message)
		}
	}
}

// TestInternalContracts_GoodFixtureClean verifies a contract.go that uses
// the canonical Service/Deps/New(Deps) Service shape produces zero findings.
func TestInternalContracts_GoodFixtureClean(t *testing.T) {
	res, err := LintInternalContracts(filepath.Join("testdata", "contracts_good"), nil)
	if err != nil {
		t.Fatalf("LintInternalContracts: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected 0 findings on canonical contract, got %d:\n%s", len(res.Findings), res.FormatText())
	}
	if res.HasErrors() {
		t.Errorf("good fixture should not have errors")
	}
}

// TestInternalContracts_BadFixtureFiresThreeFindings verifies a contract.go
// using the wrong names (Sender/Config/NewSender) produces one finding for
// each of the three canonical pieces (Service, Deps, New(Deps) Service) so
// the user sees the full delta in one run rather than discovering it
// piecemeal across re-runs.
func TestInternalContracts_BadFixtureFiresThreeFindings(t *testing.T) {
	res, err := LintInternalContracts(filepath.Join("testdata", "contracts_bad"), nil)
	if err != nil {
		t.Fatalf("LintInternalContracts: %v", err)
	}
	got := findingsForRule(res.Findings, "forgeconv-internal-package-contract-names")
	if len(got) != 3 {
		t.Fatalf("expected 3 findings (Service/Deps/New), got %d:\n%s", len(got), res.FormatText())
	}

	// Each finding must carry the same actionable phrase so users can
	// grep and find the convention doc.
	const sentinel = "internal-package contracts must declare 'type Service interface', 'type Deps struct', and 'func New(Deps) Service'"
	for _, f := range got {
		if !strings.Contains(f.Message, sentinel) {
			t.Errorf("finding missing canonical sentinel; got: %s", f.Message)
		}
		if f.Severity != SeverityError {
			t.Errorf("internal-package-contract-names should be an error, got %s", f.Severity)
		}
	}

	// The three findings should reference the three actual names found, so
	// the user sees what to rename.
	combined := res.FormatText()
	for _, want := range []string{"Sender", "Config", "NewSender"} {
		if !strings.Contains(combined, want) {
			t.Errorf("expected finding text to reference non-canonical name %q; got:\n%s", want, combined)
		}
	}

	if !res.HasErrors() {
		t.Errorf("non-canonical contract must gate the build")
	}
}

// TestInternalContracts_HonorsExcludes verifies that directories listed
// in the excludes set are skipped — packages that legitimately don't
// follow the convention (analyzer sub-packages, embed-only packages,
// internal/packs which isn't bootstrap-managed) opt out via
// contracts.exclude in forge.yaml and the analyzer must respect it.
func TestInternalContracts_HonorsExcludes(t *testing.T) {
	// First, prove the fixture would otherwise fire (no exclude → findings).
	resBefore, err := LintInternalContracts(filepath.Join("testdata", "contracts_excluded"), nil)
	if err != nil {
		t.Fatalf("LintInternalContracts: %v", err)
	}
	if len(resBefore.Findings) == 0 {
		t.Fatalf("fixture sanity: contracts_excluded must produce findings without an exclude")
	}

	// Now apply the exclude. Findings drop to zero.
	resAfter, err := LintInternalContracts(
		filepath.Join("testdata", "contracts_excluded"),
		[]string{"internal/packs"},
	)
	if err != nil {
		t.Fatalf("LintInternalContracts (excluded): %v", err)
	}
	if len(resAfter.Findings) != 0 {
		t.Fatalf("expected 0 findings with exclude, got %d:\n%s", len(resAfter.Findings), resAfter.FormatText())
	}
}

// TestInternalContracts_NoInternalDir verifies the analyzer is a no-op
// in projects without an internal/ directory (CLI/library kinds typically
// don't have one).
func TestInternalContracts_NoInternalDir(t *testing.T) {
	tmp := t.TempDir()
	res, err := LintInternalContracts(tmp, nil)
	if err != nil {
		t.Fatalf("LintInternalContracts on empty project: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("empty project should produce 0 findings, got %d", len(res.Findings))
	}
}

// TestInternalContracts_NewSignatureRejectsPointerDeps verifies a
// `func New(*Deps) Service` shape is rejected — the bootstrap template
// emits `<pkg>.New(<pkg>.Deps{...})` (a value), so a pointer receiver
// signature would compile-fail at the call site.
func TestInternalContracts_NewSignatureRejectsPointerDeps(t *testing.T) {
	tmp := t.TempDir()
	pkgDir := filepath.Join(tmp, "internal", "ptr")
	must(t, mkdirAll(pkgDir))
	must(t, writeFile(filepath.Join(pkgDir, "contract.go"), `package ptr

type Service interface { Do() error }
type Deps struct{}

// Pointer parameter — bootstrap template won't compile against this.
func New(d *Deps) Service { return nil }
`))
	res, err := LintInternalContracts(tmp, nil)
	if err != nil {
		t.Fatalf("LintInternalContracts: %v", err)
	}
	got := findingsForRule(res.Findings, "forgeconv-internal-package-contract-names")
	if len(got) != 1 {
		t.Fatalf("expected 1 finding for ptr-Deps mismatch, got %d:\n%s", len(got), res.FormatText())
	}
	if !strings.Contains(got[0].Message, "New") {
		t.Errorf("finding should reference New constructor; got: %s", got[0].Message)
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
