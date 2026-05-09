package forgeconv

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLintInteractorDepsAreInterfaces_Fires verifies the rule fires
// when an interactor's Deps struct holds a concrete struct pointer.
// The pattern defeats the all-mock-test surface that interactors are
// designed for.
func TestLintInteractorDepsAreInterfaces_Fires(t *testing.T) {
	t.Parallel()
	res, err := LintInteractorDepsAreInterfaces(filepath.Join("testdata", "interactor_concrete_deps"))
	if err != nil {
		t.Fatalf("LintInteractorDepsAreInterfaces: %v", err)
	}
	got := findingsForRule(res.Findings, "forgeconv-interactor-deps-are-interfaces")
	if len(got) != 1 {
		t.Fatalf("expected 1 finding (Charger field), got %d:\n%s", len(got), res.FormatText())
	}
	f := got[0]
	if f.Severity != SeverityWarning {
		t.Errorf("rule should be a warning, got %s", f.Severity)
	}
	if !strings.Contains(f.Message, "Charger") {
		t.Errorf("message should name the offending field (Charger); got: %s", f.Message)
	}
	if !strings.Contains(f.Message, "interface") {
		t.Errorf("message should mention 'interface'; got: %s", f.Message)
	}
	if !strings.Contains(f.Remediation, "forge skill load interactor") {
		t.Errorf("remediation should point at the interactor skill; got: %s", f.Remediation)
	}
	if res.HasErrors() {
		t.Errorf("rule must not gate the build; HasErrors() = true")
	}
}

// TestLintInteractorDepsAreInterfaces_CleanFixture verifies a
// well-formed interactor (Deps fields are interfaces, plus the
// always-allowed Logger) produces no findings.
func TestLintInteractorDepsAreInterfaces_CleanFixture(t *testing.T) {
	t.Parallel()
	res, err := LintInteractorDepsAreInterfaces(filepath.Join("testdata", "interactor_clean"))
	if err != nil {
		t.Fatalf("LintInteractorDepsAreInterfaces: %v", err)
	}
	got := findingsForRule(res.Findings, "forgeconv-interactor-deps-are-interfaces")
	if len(got) != 0 {
		t.Fatalf("expected 0 findings on clean fixture, got %d:\n%s", len(got), res.FormatText())
	}
}

// TestLintInteractorDepsAreInterfaces_NoInternalDir confirms projects
// without an internal/ tree (CLI / library kinds) get an empty result
// rather than an error.
func TestLintInteractorDepsAreInterfaces_NoInternalDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	res, err := LintInteractorDepsAreInterfaces(tmp)
	if err != nil {
		t.Fatalf("LintInteractorDepsAreInterfaces on empty project: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("empty project should produce 0 findings, got %d", len(res.Findings))
	}
}
