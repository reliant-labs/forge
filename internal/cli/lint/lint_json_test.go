package lint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/linter/finding"
)

// TestLintJSONReportShape pins the exact serialized shape of the
// `forge lint --json` contract — field names, omission rules, key
// order. A failure here means a breaking change for every sub-agent /
// CI consumer parsing the report; extensions must be additive.
func TestLintJSONReportShape(t *testing.T) {
	report := buildLintJSONReport([]lintJSONFinding{
		{
			File:     "handlers/api/handlers.go",
			Line:     42,
			Col:      7,
			Severity: lintSevError,
			Rule:     "forgeconv-contract-names",
			Message:  "contract.go declares 'Sender', expected 'Service'",
			FixHint:  "rename the interface to Service",
		},
		{
			// File-less finding: file/line/col/fix_hint must be omitted.
			Severity: lintSevWarning,
			Rule:     "external",
			Message:  "some raw sub-tool line",
		},
		{
			Severity: lintSevInfo,
			Rule:     "skipped",
			Message:  "buf not found on PATH — skipping buf lint",
		},
	}, true)

	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := `{
  "findings": [
    {
      "file": "handlers/api/handlers.go",
      "line": 42,
      "col": 7,
      "severity": "error",
      "rule": "forgeconv-contract-names",
      "message": "contract.go declares 'Sender', expected 'Service'",
      "fix_hint": "rename the interface to Service"
    },
    {
      "severity": "warning",
      "rule": "external",
      "message": "some raw sub-tool line"
    },
    {
      "severity": "info",
      "rule": "skipped",
      "message": "buf not found on PATH — skipping buf lint"
    }
  ],
  "summary": {
    "errors": 1,
    "warnings": 1,
    "infos": 1,
    "total": 3
  },
  "ok": false
}`
	if string(got) != want {
		t.Errorf("JSON shape drifted.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestLintJSONReportEmptyFindings pins the empty-report shape: findings
// must serialize as [] (not null) so `jq '.findings[]'` never chokes,
// and ok must be true when nothing gated.
func TestLintJSONReportEmptyFindings(t *testing.T) {
	report := buildLintJSONReport(nil, false)
	got, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"findings":[],"summary":{"errors":0,"warnings":0,"infos":0,"total":0},"ok":true}`
	if string(got) != want {
		t.Errorf("empty report drifted.\ngot:  %s\nwant: %s", got, want)
	}
}

// TestLintJSONOKIndependentOfWarnings pins the gating contract: ok
// reflects the text-mode exit code, NOT the presence of warnings. A
// report full of warnings from advisory linters stays ok=true.
func TestLintJSONOKIndependentOfWarnings(t *testing.T) {
	report := buildLintJSONReport([]lintJSONFinding{
		{Severity: lintSevWarning, Rule: "forge-wire-coverage", Message: "w"},
		{Severity: lintSevWarning, Rule: "forge-test-conventions", Message: "w"},
	}, false)
	if !report.OK {
		t.Error("warnings-only report must keep ok=true (advisory linters never gate)")
	}
	if report.Summary.Warnings != 2 || report.Summary.Errors != 0 {
		t.Errorf("unexpected summary: %+v", report.Summary)
	}
}

// TestExternalLinesToFindings is the table for sub-tool output
// normalization: parseable `file:line[:col]: message` lines become
// file-scoped findings; everything else is preserved verbatim with
// rule "external"; blank lines are dropped (but nothing else is).
func TestExternalLinesToFindings(t *testing.T) {
	cases := []struct {
		name     string
		output   string
		rule     string
		severity string
		want     []lintJSONFinding
	}{
		{
			name:     "golangci line with trailing linter attribution",
			output:   "internal/cli/lint.go:12:5: ineffectual assignment to err (ineffassign)",
			rule:     "golangci-lint",
			severity: lintSevError,
			want: []lintJSONFinding{{
				File: "internal/cli/lint.go", Line: 12, Col: 5,
				Severity: lintSevError, Rule: "ineffassign",
				Message: "ineffectual assignment to err (ineffassign)",
			}},
		},
		{
			name:     "buf line without column keeps the buf rule",
			output:   "proto/api/v1/api.proto:3:1:Files with package \"api.v1\" must be within a directory \"api/v1\".",
			rule:     "buf",
			severity: lintSevError,
			want: []lintJSONFinding{{
				File: "proto/api/v1/api.proto", Line: 3, Col: 1,
				Severity: lintSevError, Rule: "buf",
				Message: "Files with package \"api.v1\" must be within a directory \"api/v1\".",
			}},
		},
		{
			name:     "unparseable line preserved verbatim as external",
			output:   "npm ERR! Lifecycle script `lint` failed with error:",
			rule:     "golangci-lint",
			severity: lintSevError,
			want: []lintJSONFinding{{
				Severity: lintSevError, Rule: "external",
				Message: "npm ERR! Lifecycle script `lint` failed with error:",
			}},
		},
		{
			name:     "blank lines dropped, mixed parse",
			output:   "\n\nmain.go:1:1: missing package comment\n\nsome banner\n",
			rule:     "golangci-lint",
			severity: lintSevError,
			want: []lintJSONFinding{
				{File: "main.go", Line: 1, Col: 1, Severity: lintSevError, Rule: "golangci-lint", Message: "missing package comment"},
				{Severity: lintSevError, Rule: "external", Message: "some banner"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := externalLinesToFindings(tc.output, tc.rule, tc.severity)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d findings, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("finding[%d]:\ngot:  %+v\nwant: %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestParseSeverity covers the canonical severity vocabulary that
// replaced the old normalizeLintSeverity shim. The internal linters now
// emit finding.Severity directly (single spelling), so the only
// normalization left is parsing free-form forge.yaml rule levels: "warn"
// is accepted as a legacy alias for "warning", and anything unrecognized
// returns ("", false) — the disabled-rule sentinel — rather than the old
// shim's "degrade to warning" fallback (which has been deleted).
func TestParseSeverity(t *testing.T) {
	okCases := []struct {
		in   string
		want finding.Severity
	}{
		{"error", finding.SeverityError},
		{"warn", finding.SeverityWarning},
		{"warning", finding.SeverityWarning},
		{"info", finding.SeverityInfo},
	}
	for _, c := range okCases {
		got, ok := finding.ParseSeverity(c.in)
		if !ok || got != c.want {
			t.Errorf("ParseSeverity(%q) = (%q, %v), want (%q, true)", c.in, got, ok, c.want)
		}
	}
	if got, ok := finding.ParseSeverity("WEIRD"); ok {
		t.Errorf("ParseSeverity(%q) = (%q, true), want (\"\", false)", "WEIRD", got)
	}
}

// TestCollectWireCoverageJSON exercises a structured collector end to
// end against a synthetic project: one wire TODO (warning, no gate) and
// one unresolved forge:placeholder (error, gates) — mirroring text
// mode's "TODOs warn, placeholders fail" split.
func TestCollectWireCoverageJSON(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "pkg", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wireGen := `package app

func wireBillingDeps(app *App) billing.Deps {
	return billing.Deps{
		Repo: nil, // TODO: wire Repo
	}
}
`
	extras := `package app

// AppExtras holds user-owned dependency fields.
type AppExtras struct {
	// forge:placeholder: billing.Repository
	Repo any
}
`
	if err := os.WriteFile(filepath.Join(appDir, "wire_gen.go"), []byte(wireGen), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "app_extras.go"), []byte(extras), 0o644); err != nil {
		t.Fatal(err)
	}

	findings, gated, err := collectWireCoverageJSON(root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !gated {
		t.Error("unresolved placeholder must gate (text mode returns an error)")
	}
	var warnings, errors int
	for _, f := range findings {
		if f.Rule != "forge-wire-coverage" {
			t.Errorf("unexpected rule %q", f.Rule)
		}
		switch f.Severity {
		case lintSevWarning:
			warnings++
			if f.File == "" || f.Line == 0 {
				t.Errorf("TODO finding must be file-scoped, got %+v", f)
			}
		case lintSevError:
			errors++
			if !strings.Contains(f.Message, "forge:placeholder") {
				t.Errorf("placeholder finding message drifted: %q", f.Message)
			}
		}
		if f.FixHint == "" {
			t.Errorf("wire-coverage findings must carry a fix_hint, got %+v", f)
		}
	}
	if warnings != 1 || errors != 1 {
		t.Errorf("expected 1 warning + 1 error, got %d warnings, %d errors: %+v", warnings, errors, findings)
	}
}

// TestRunLintJSONRejectsSuggestionModes pins the refusal: --json with
// the suggestion / mutation flags must error out instead of emitting a
// report that silently ignored the flag.
func TestRunLintJSONRejectsSuggestionModes(t *testing.T) {
	for _, f := range []lintFlags{
		{fix: true, jsonOut: true},
		{suggestExcludes: true, jsonOut: true},
		{suggestBufExcepts: true, jsonOut: true},
	} {
		if err := runLintJSON(context.Background(), f, []string{"./..."}); err == nil {
			t.Errorf("expected error for flags %+v with --json", f)
		}
	}
}
