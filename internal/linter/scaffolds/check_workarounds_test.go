package scaffolds

import (
	"path/filepath"
	"testing"
)

func TestLintWorkaroundsRoot_Clean(t *testing.T) {
	t.Parallel()
	res, err := LintWorkaroundsRoot(filepath.Join("testdata", "check_workarounds", "clean"))
	if err != nil {
		t.Fatalf("LintWorkaroundsRoot returned error: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected zero findings on clean fixture, got %d: %+v", len(res.Findings), res.Findings)
	}
	// Workaround findings are warnings only; the clean fixture must
	// also not trip HasErrors().
	if res.HasErrors() {
		t.Fatal("clean fixture must not produce errors")
	}
}

func TestLintWorkaroundsRoot_Firing(t *testing.T) {
	t.Parallel()
	res, err := LintWorkaroundsRoot(filepath.Join("testdata", "check_workarounds", "firing"))
	if err != nil {
		t.Fatalf("LintWorkaroundsRoot returned error: %v", err)
	}
	wantRules := map[string]bool{
		"workaround-wire-cast-helper":   false,
		"workaround-testing-extras":     false,
		"workaround-cmd-not-in-binaries": false,
	}
	for _, f := range res.Findings {
		if _, ok := wantRules[f.Rule]; ok {
			wantRules[f.Rule] = true
		}
		// All workaround findings must be warnings.
		if f.Severity != SeverityWarning {
			t.Errorf("rule %s: expected severity %q, got %q", f.Rule, SeverityWarning, f.Severity)
		}
	}
	for rule, fired := range wantRules {
		if !fired {
			t.Errorf("expected rule %s to fire on firing fixture, got findings: %+v", rule, res.Findings)
		}
	}
	// Workaround findings never gate the build.
	if res.HasErrors() {
		t.Fatal("workaround findings must be warnings, not errors")
	}
}

func TestReadDeclaredBinaries(t *testing.T) {
	t.Parallel()
	got := readDeclaredBinaries(filepath.Join("testdata", "check_workarounds", "clean", "forge.yaml"))
	if !got["server"] {
		t.Errorf("expected server in declared binaries, got %+v", got)
	}
	if !got["workspace-proxy"] {
		t.Errorf("expected workspace-proxy in declared binaries, got %+v", got)
	}
}

func TestIsExemptCmdFile(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"server":          true,
		"root":            true,
		"version":         true,
		"main":            true,
		"db":              true,
		"otel":            true,
		"_shared":         true,
		"foo_shared":      true,
		"workspace_proxy": false,
		"extra":           false,
	}
	for in, want := range cases {
		if got := isExemptCmdFile(in); got != want {
			t.Errorf("isExemptCmdFile(%q) = %v, want %v", in, got, want)
		}
	}
}
