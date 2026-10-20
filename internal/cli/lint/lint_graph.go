// File: internal/cli/lint/lint_graph.go
//
// graph — `forge lint --graph` (composition graph PR 5). The whole-graph
// checks generate cannot make one node at a time: every component and
// provider reachable from some root, and every listener's services in the
// audience of its chain's authenticator. The engine is
// codegen.LintGraph, which judges the graph exactly as `forge generate`
// assembles it; this file renders it. Errors gate: in graph mode they are
// what the boot-time RequireComplete gate used to catch.

package lint

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/reliant-labs/forge/internal/codegen"
)

// runGraphLint is the text-mode entry point.
func runGraphLint(projectDir string) error {
	fmt.Println("Running composition-graph lint...")
	findings, graphMode, err := codegen.LintGraph(projectDir)
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
func collectGraphJSON(projectDir string) ([]lintJSONFinding, error) {
	findings, _, err := codegen.LintGraph(projectDir)
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
