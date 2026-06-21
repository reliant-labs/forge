package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cli/audittype"
)

// TestAuditFileSizes_FlagsOversizedFile fixtures a .go file past the line
// threshold and confirms it surfaces as a warn with the file listed.
func TestAuditFileSizes_FlagsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "internal", "db")
	if err := os.MkdirAll(big, 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("package db\n")
	for i := 0; i < auditFileLineWarn+10; i++ {
		fmt.Fprintf(&sb, "// line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(big, "postgres.go"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	cat := auditFileSizes(dir)
	if cat.Status != audittype.StatusWarn {
		t.Fatalf("status = %q, want warn (summary=%q)", cat.Status, cat.Summary)
	}
	if !strings.Contains(cat.Summary, "oversized") {
		t.Errorf("summary should mention oversized files: %q", cat.Summary)
	}
}

// TestAuditFileSizes_FlagsGodObjectType fixtures a type with more than the
// method threshold and confirms it's flagged as a god-object.
func TestAuditFileSizes_FlagsGodObjectType(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "internal", "repo")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("package repo\n\ntype Repo struct{}\n\n")
	for i := 0; i < auditTypeMethodWarn+5; i++ {
		fmt.Fprintf(&sb, "func (r *Repo) M%d() {}\n", i)
	}
	if err := os.WriteFile(filepath.Join(pkg, "repo.go"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	cat := auditFileSizes(dir)
	if cat.Status != audittype.StatusWarn {
		t.Fatalf("status = %q, want warn (summary=%q)", cat.Status, cat.Summary)
	}
	if !strings.Contains(cat.Summary, "god-object") {
		t.Errorf("summary should mention god-object: %q", cat.Summary)
	}
}

// TestAuditFileSizes_CleanProject confirms a small project is ok.
func TestAuditFileSizes_CleanProject(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "internal", "small")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "small.go"), []byte("package small\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := auditFileSizes(dir)
	if cat.Status != audittype.StatusOK {
		t.Errorf("status = %q, want ok (summary=%q)", cat.Status, cat.Summary)
	}
}

// writeHandlerFile builds a handlers.go body where named methods are stubs
// (carry the unwired-stub marker) and the rest are implemented.
func writeHandlerFile(t *testing.T, dir, pkg string, stubs, real []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "package %s\n\ntype Service struct{}\n\n", pkg)
	for _, m := range stubs {
		fmt.Fprintf(&sb, "// forge:gen unwired-stub symbol=%s.%s\nfunc (s *Service) %s() error {\n\treturn nil\n}\n\n", pkg, m, m)
	}
	for _, m := range real {
		fmt.Fprintf(&sb, "// %s does real work.\nfunc (s *Service) %s() error {\n\treturn nil\n}\n\n", m, m)
	}
	if err := os.WriteFile(filepath.Join(dir, "handlers.go"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAuditOrphanStubs_FlagsAllStubService confirms a service whose every
// handler is an un-implemented stub is flagged.
func TestAuditOrphanStubs_FlagsAllStubService(t *testing.T) {
	dir := t.TempDir()
	writeHandlerFile(t, filepath.Join(dir, "internal", "handlers", "reporting"), "reporting",
		[]string{"GetReport", "ListReports"}, nil)

	cat := auditOrphanStubs(testFactory(auditAPIConfig{}), nil, dir)
	if cat.Status != audittype.StatusWarn {
		t.Fatalf("status = %q, want warn (summary=%q)", cat.Status, cat.Summary)
	}
	if !strings.Contains(cat.Summary, "un-implemented") {
		t.Errorf("summary should flag un-implemented service: %q", cat.Summary)
	}
}

// TestAuditOrphanStubs_PartialImplNotFlagged confirms a service with at
// least one real handler is NOT flagged.
func TestAuditOrphanStubs_PartialImplNotFlagged(t *testing.T) {
	dir := t.TempDir()
	writeHandlerFile(t, filepath.Join(dir, "internal", "handlers", "billing"), "billing",
		[]string{"Refund"}, []string{"Charge"})

	cat := auditOrphanStubs(testFactory(auditAPIConfig{}), nil, dir)
	if cat.Status != audittype.StatusOK {
		t.Fatalf("status = %q, want ok — partial impl is not an orphan (summary=%q)", cat.Status, cat.Summary)
	}
}

// TestAuditOrphanStubs_NoHandlersDir confirms a project without handlers/
// reports n/a rather than warning.
func TestAuditOrphanStubs_NoHandlersDir(t *testing.T) {
	cat := auditOrphanStubs(testFactory(auditAPIConfig{}), nil, t.TempDir())
	if cat.Status != audittype.StatusOK {
		t.Errorf("status = %q, want ok for no handlers dir", cat.Status)
	}
}
