// File: internal/cli/lint/lint_tracing.go
//
// The tracing lane: `forge lint --tracing`, and the same rules in the full
// sweep. The rules live in internal/linter/tracinglint; this file wires them
// to forge.yaml (lint.tracing.log_wrappers, lint.rules), to the suppression
// directives, and to forge's own instrument decision for contract packages.
//
// Autofix. forge-tracing-log-without-ctx has a mechanical fix, applied:
//
//   - by `forge lint --tracing` (unless --no-fix) at any severity but off —
//     naming the lane is the request to migrate;
//   - by the full sweep only where the rule is at error, so pinning a forge
//     that adds the rule to a repo that dialled it to warn rewrites nothing
//     until that repo runs the migration itself.
//
// A finding a suppression directive covers is never rewritten.

package lint

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/finding"
	"github.com/reliant-labs/forge/internal/linter/suppress"
	"github.com/reliant-labs/forge/internal/linter/tracinglint"
)

// tracingRun is one evaluation of the tracing rules.
type tracingRun struct {
	findings []finding.Finding
	fixed    []tracinglint.Fixed
}

// tracingFindings runs the rules over the project at cwd. fix enables the
// autofix; explicit says the lane was named (`--tracing`), which fixes at
// warn too.
func tracingFindings(cwd string, cfg *config.ProjectConfig, fix, explicit bool) (tracingRun, error) {
	var lintCfg config.LintConfig
	contracts := true
	if cfg != nil {
		lintCfg = cfg.Lint
		contracts = cfg.Features.ContractsEnabled()
	}
	rules := lintCfg.EffectiveRules()

	wrappers := make([]tracinglint.LogWrapper, 0, len(lintCfg.Tracing.LogWrappers))
	for i, w := range lintCfg.Tracing.LogWrappers {
		lw := tracinglint.LogWrapper{Call: w.Call, ContextCall: w.ContextCall}
		if err := tracinglint.CheckLogWrapper(lw); err != nil {
			return tracingRun{}, fmt.Errorf("forge.yaml lint.tracing.log_wrappers[%d]: %w", i, err)
		}
		wrappers = append(wrappers, lw)
	}

	opts := tracinglint.Options{
		LogWrappers: wrappers,
		Fix:         fix,
		FixFilter: func(f finding.Finding) bool {
			adjusted, keep := rules.Override(f)
			if !keep || (!explicit && adjusted.Severity != finding.SeverityError) {
				return false
			}
			content, err := os.ReadFile(filepath.Join(cwd, filepath.FromSlash(f.File)))
			if err != nil {
				return false
			}
			return len(suppress.Apply(string(content), []finding.Finding{f}).Kept) == 1
		},
	}
	if contracts {
		opts.Instrumented = instrumentedComponent
	}
	res, err := tracinglint.Lint(cwd, opts)
	if err != nil {
		return tracingRun{}, fmt.Errorf("tracing lint: %w", err)
	}
	applied := suppress.ApplyFiles(cwd, res.Findings, os.ReadFile)
	kept := rules.ApplyAll(applied.Kept)
	kept = append(kept, applied.Violations...)
	return tracingRun{findings: kept, fixed: res.Fixed}, nil
}

// instrumentedComponent is forge generate's instrument decision for the
// package in dir: the same function the compose assembler consults, so the
// lint and the generated wiring cannot disagree.
func instrumentedComponent(dir string) bool {
	ctorType, err := codegen.DetectConstructorType(dir)
	if err != nil || ctorType == "" {
		return false
	}
	iface := codegen.DetectServiceInterfaceName(dir)
	if iface == "" {
		iface = "Service"
	}
	return codegen.ShouldInstrumentComponent(dir, ctorType, iface)
}

// runTracingLint is the text arm.
func runTracingLint(cwd string, cfg *config.ProjectConfig, fix, explicit bool) error {
	fmt.Println("Running tracing lint...")
	run, err := tracingFindings(cwd, cfg, fix, explicit)
	if err != nil {
		return err
	}
	rewrote := 0
	for _, f := range run.fixed {
		rewrote += f.Count
		fmt.Printf("  ✎ %s: %d log call(s) rewritten to pass the request context\n", f.File, f.Count)
	}
	if len(run.findings) == 0 {
		if rewrote > 0 {
			fmt.Printf("✓ tracing lint clean after rewriting %d log call(s)\n", rewrote)
		} else {
			fmt.Println("✓ tracing lint clean — logs carry their context; HTTP clients, database pools and contracts are traced")
		}
		return nil
	}
	errs, logWarns := 0, 0
	for _, f := range run.findings {
		icon := "⚠"
		if f.Severity == finding.SeverityError {
			icon = "❌"
			errs++
		} else if f.Rule == tracinglint.RuleLogWithoutCtx {
			logWarns++
		}
		fmt.Printf("  %s [%s] %s:%d\n      %s\n", icon, f.Rule, f.File, f.Line, f.Message)
		if f.Remediation != "" {
			fmt.Printf("      → %s\n", f.Remediation)
		}
	}
	if logWarns > 0 && !explicit {
		fmt.Printf("\n  %d log call(s) can be rewritten automatically: run `forge lint --tracing`.\n", logWarns)
	}
	if errs == 0 {
		fmt.Println("(warnings only — not failing the build)")
		return nil
	}
	return cliutil.UserErr("forge lint --tracing",
		fmt.Sprintf("%d place(s) where a request's trace is cut", errs), "",
		"fix each finding above, or suppress one with `// forge:lint-disable-next-line <rule>: <reason>`; "+
			"to adopt a rule gradually set it to warn under forge.yaml lint.rules — see 'forge skill load observability'")
}

// collectTracingJSON is the JSON arm. It never rewrites: --json is
// detect-only. Error findings gate.
func collectTracingJSON(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
	run, err := tracingFindings(rc.cwd, rc.cfg, false, false)
	if err != nil {
		return nil, false, err
	}
	out := findingsToJSON(run.findings)
	return out, anyErrorFinding(out), nil
}

// hasGoModule reports whether the tracing lane has Go source to judge.
func hasGoModule(cwd string) bool {
	_, err := os.Stat(filepath.Join(cwd, "go.mod"))
	return err == nil
}
