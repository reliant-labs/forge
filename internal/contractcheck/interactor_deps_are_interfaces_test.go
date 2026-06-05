// File: internal/contractcheck/interactor_deps_are_interfaces_test.go
//
// Ported from internal/linter/forgeconv/interactor_deps_are_interfaces_test.go
// on 2026-06-04. Fixtures and assertions are unchanged.

package contractcheck

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/linter/forgeconv"
)

// TestLintInteractorDepsAreInterfaces_Fires verifies the rule fires
// when an interactor's Deps struct holds a concrete struct pointer.
// The pattern defeats the all-mock-test surface that interactors are
// designed for.
func TestLintInteractorDepsAreInterfaces_Fires(t *testing.T) {
	t.Parallel()
	fs, err := Inspect(context.Background(),
		filepath.Join("testdata", "interactor_concrete_deps"),
		Options{Rules: []Rule{RuleInteractorDepsAreInterfaces}},
	)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	got := findingsForRule(fs, string(RuleInteractorDepsAreInterfaces))
	if len(got) != 1 {
		t.Fatalf("expected 1 finding (Charger field), got %d:\n%s",
			len(got), AsResult(fs).FormatText())
	}
	f := got[0]
	if f.Severity != forgeconv.SeverityWarning {
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
	if HasErrors(fs) {
		t.Errorf("rule must not gate the build; HasErrors() = true")
	}
}

// TestLintInteractorDepsAreInterfaces_CleanFixture verifies a
// well-formed interactor (Deps fields are interfaces, plus the
// always-allowed Logger) produces no findings.
func TestLintInteractorDepsAreInterfaces_CleanFixture(t *testing.T) {
	t.Parallel()
	fs, err := Inspect(context.Background(),
		filepath.Join("testdata", "interactor_clean"),
		Options{Rules: []Rule{RuleInteractorDepsAreInterfaces}},
	)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	got := findingsForRule(fs, string(RuleInteractorDepsAreInterfaces))
	if len(got) != 0 {
		t.Fatalf("expected 0 findings on clean fixture, got %d:\n%s",
			len(got), AsResult(fs).FormatText())
	}
}

// TestLintInteractorDepsAreInterfaces_NoInternalDir confirms projects
// without an internal/ tree (CLI / library kinds) get an empty result
// rather than an error.
func TestLintInteractorDepsAreInterfaces_NoInternalDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	fs, err := Inspect(context.Background(), tmp,
		Options{Rules: []Rule{RuleInteractorDepsAreInterfaces}},
	)
	if err != nil {
		t.Fatalf("Inspect on empty project: %v", err)
	}
	if len(fs) != 0 {
		t.Errorf("empty project should produce 0 findings, got %d", len(fs))
	}
}
