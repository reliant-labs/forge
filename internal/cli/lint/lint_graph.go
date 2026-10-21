// File: internal/cli/lint/lint_graph.go
//
// graph — `forge lint --graph` (composition graph PR 5). The whole-graph
// checks generate cannot make one node at a time: every component and
// provider reachable from some root, and every listener's services in the
// audience of its chain's authenticator. The engine is
// codegen.LintGraph, which judges the graph exactly as `forge generate`
// assembles it; this file renders it. Errors gate: in graph mode they are
// what the boot-time RequireComplete gate used to catch.
//
// Severity is the analyzer's, re-graded by forge.yaml's `lint.rules` — the
// same dial every forge rule answers to — so a project adopting roots one at
// a time sets `lint.rules: {forge-graph-unreachable: warn}` until the last
// root is declared, then deletes the line.

package lint

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/finding"
	"github.com/reliant-labs/forge/internal/linter/suppress"
)

// runGraphLint is the text-mode entry point.
func runGraphLint(projectDir string, cfg *config.ProjectConfig) error {
	fmt.Println("Running composition-graph lint...")
	findings, graphMode, err := graphFindings(projectDir, cfg)
	if err != nil {
		return err
	}
	if !graphMode {
		fmt.Println("  graph lint skipped — internal/app is the legacy Infra / NewComponents composition, not the composition graph")
		return nil
	}
	if formatGraphFindings(os.Stdout, findings) {
		return errors.New("the composition graph has errors (see above)")
	}
	return nil
}

// graphFindings judges the graph and re-grades each finding through
// lint.rules.
func graphFindings(projectDir string, cfg *config.ProjectConfig) ([]codegen.GraphFinding, bool, error) {
	findings, graphMode, err := codegen.LintGraph(projectDir)
	if err != nil || cfg == nil {
		return findings, graphMode, err
	}
	return regradeGraphFindings(findings, cfg.Lint.EffectiveRules()), graphMode, nil
}

// regradeGraphFindings applies lint.rules to graph findings through the
// engine every forge rule shares: `off` drops a finding, `warn` keeps it
// without gating, `error` gates, and an exact rule id beats `*`.
func regradeGraphFindings(findings []codegen.GraphFinding, rules suppress.RuleSeverities) []codegen.GraphFinding {
	if len(rules) == 0 {
		return findings
	}
	out := make([]codegen.GraphFinding, 0, len(findings))
	for _, f := range findings {
		sev := finding.SeverityWarning
		if f.Error {
			sev = finding.SeverityError
		}
		adjusted, keep := rules.Override(finding.Finding{Rule: f.Rule, Severity: sev, File: f.File, Line: f.Line, Message: f.Message})
		if !keep {
			continue
		}
		f.Error = adjusted.Severity == finding.SeverityError
		out = append(out, f)
	}
	return out
}

// formatGraphFindings writes the human report and reports whether any
// finding is an error.
func formatGraphFindings(w io.Writer, findings []codegen.GraphFinding) bool {
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "  graph clean — every component and provider is reached by a root, and every listener serves its chain's audience")
		return false
	}
	failed := false
	for _, f := range findings {
		mark := "⚠"
		if f.Error {
			mark, failed = "✗", true
		}
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		_, _ = fmt.Fprintf(w, "  %s [%s] %s: %s\n", mark, f.Rule, loc, f.Message)
		if f.Fix != "" {
			_, _ = fmt.Fprintf(w, "      → %s\n", f.Fix)
		}
	}
	return failed
}

// collectGraphJSON maps graph findings onto the JSON contract; an error
// finding gates.
func collectGraphJSON(projectDir string, cfg *config.ProjectConfig) ([]lintJSONFinding, error) {
	findings, _, err := graphFindings(projectDir, cfg)
	if err != nil {
		return nil, fmt.Errorf("graph lint failed: %w", err)
	}
	out := make([]lintJSONFinding, 0, len(findings))
	for _, f := range findings {
		sev := lintSevWarning
		if f.Error {
			sev = lintSevError
		}
		out = append(out, lintJSONFinding{File: f.File, Line: f.Line, Severity: sev, Rule: f.Rule, Message: f.Message, FixHint: f.Fix})
	}
	return out, nil
}
