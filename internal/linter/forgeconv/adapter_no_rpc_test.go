package forgeconv

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLintAdapterNoRPC_Fires verifies the rule fires when a
// `// forge:adapter`-marked package registers a Connect RPC handler.
// The adapter convention is outbound-only — RPC means it should be a
// service.
func TestLintAdapterNoRPC_Fires(t *testing.T) {
	t.Parallel()
	res, err := LintAdapterNoRPC(filepath.Join("testdata", "adapter_with_rpc"))
	if err != nil {
		t.Fatalf("LintAdapterNoRPC: %v", err)
	}
	got := findingsForRule(res.Findings, "forgeconv-adapter-no-rpc")
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d:\n%s", len(got), res.FormatText())
	}
	f := got[0]
	if f.Severity != SeverityWarning {
		t.Errorf("rule should be a warning, got %s", f.Severity)
	}
	if !strings.Contains(f.Message, "forge:adapter") {
		t.Errorf("message should mention `forge:adapter`; got: %s", f.Message)
	}
	if !strings.Contains(f.Message, "NewBillingHandler") {
		t.Errorf("message should name the offending Connect handler; got: %s", f.Message)
	}
	if !strings.Contains(f.Remediation, "forge skill load adapter") {
		t.Errorf("remediation should point at the adapter skill; got: %s", f.Remediation)
	}
	// Warnings must not gate the build.
	if res.HasErrors() {
		t.Errorf("rule must not gate the build; HasErrors() = true")
	}
}

// TestLintAdapterNoRPC_CleanFixture verifies a properly outbound-only
// adapter package produces no findings.
func TestLintAdapterNoRPC_CleanFixture(t *testing.T) {
	t.Parallel()
	res, err := LintAdapterNoRPC(filepath.Join("testdata", "adapter_clean"))
	if err != nil {
		t.Fatalf("LintAdapterNoRPC: %v", err)
	}
	got := findingsForRule(res.Findings, "forgeconv-adapter-no-rpc")
	if len(got) != 0 {
		t.Fatalf("expected 0 findings on clean fixture, got %d:\n%s", len(got), res.FormatText())
	}
}

// TestLintAdapterNoRPC_NoInternalDir confirms projects without an
// internal/ tree (CLI / library kinds) get an empty result rather than
// an error.
func TestLintAdapterNoRPC_NoInternalDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	res, err := LintAdapterNoRPC(tmp)
	if err != nil {
		t.Fatalf("LintAdapterNoRPC on empty project: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("empty project should produce 0 findings, got %d", len(res.Findings))
	}
}
