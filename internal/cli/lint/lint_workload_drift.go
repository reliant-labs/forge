// File: internal/cli/lint/lint_component_drift.go
//
// component-drift — reports disagreement between what the project's CODE
// declares and what `deploy/kcl/workloads.k` declares.
//
// forge does not regenerate workloads.k. It is scaffolded once, appended to
// by `forge scaffold <kind>`, and owned by the project from then on — because
// deploy is where hyper-customization per environment lives, and a generator
// that rewrote it would overwrite exactly the edits it exists to enable.
//
// The cost of that choice is drift: a service can be added to the protos (or
// a worker package created by hand, or a component deleted) without the
// deploy declaration following. This lint is what pays that cost. It REPORTS,
// naming the component and printing the literal stanza to paste — it never
// writes.
//
// WARNING, NOT ERROR, and the asymmetry is deliberate:
//
//   - A component in the code with no entry here is usually an oversight, but
//     it is also how you legitimately ship a component NO environment
//     deploys — one still being built, or one deployed by something outside
//     forge entirely.
//   - An entry here naming no component in the code is usually stale, but it
//     is also how you legitimately declare infrastructure forge did not
//     generate: NATS, a cache, a third-party sidecar. Those have no Go
//     package to discover and MUST NOT be reported as errors.
//
// Neither direction can be distinguished from a deliberate choice by reading
// the tree, so neither fails a build. Forge notices; the human decides.

package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

// componentDriftFinding is one disagreement between the code and the deploy
// declaration. Undeclared is true when the code has a workload workloads.k
// does not (the paste-this-stanza case); false when workloads.k names one
// the code does not.
type componentDriftFinding struct {
	Name       string
	Kind       string
	Undeclared bool
	Stanza     string
}

// workloadNameInKCL matches a `name = "<x>"` field — the component-name
// declaration inside a typed literal. Anchored on the field rather than a
// bare substring so a name appearing in a command path or an image reference
// does not read as a declaration.
var workloadNameInKCL = regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"\s*$`)

// declaredWorkloadNames returns the workload names a workloads.k actually
// declares.
//
// Docstrings and comments are stripped first (codegen.StripKCLProse — the
// same rule the append path's duplicate check uses), because the scaffolded
// workloads.k DOCUMENTS the shape it expects, including a worked example
// declaring `name = "nats"`. Without this, that example reads as a real
// component and the lint reports an orphan entry for something nobody
// declared. Prose that teaches the format must not be mistaken for the format.
func declaredWorkloadNames(src string) map[string]bool {
	out := map[string]bool{}
	for _, m := range workloadNameInKCL.FindAllStringSubmatch(codegen.StripKCLProse(src), -1) {
		out[m[1]] = true
	}
	return out
}

// collectComponentDrift compares the project's discovered components against
// the names declared in deploy/kcl/workloads.k.
//
// A project with no workloads.k yields NO findings: that is a project whose
// deploy is not scaffolded (features.deploy off, or a CLI/library kind), and
// reporting every component as undeclared there would be noise.
func collectComponentDrift(projectDir string, cfg *config.ProjectConfig) ([]componentDriftFinding, error) {
	if cfg == nil {
		return nil, nil
	}
	path := filepath.Join(projectDir, codegen.WorkloadsKCLRelPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	declared := declaredWorkloadNames(string(raw))

	inCode := map[string]bool{}
	var findings []componentDriftFinding
	for _, c := range codegen.DiscoverProjectComponents(projectDir, cfg.Name) {
		inCode[c.Name] = true
		if declared[c.Name] {
			continue
		}
		findings = append(findings, componentDriftFinding{
			Name:       c.Name,
			Kind:       c.EffectiveKind(),
			Undeclared: true,
			Stanza:     codegen.WorkloadStanza(cfg.Name, c),
		})
	}

	// The other direction. Entries with no Go component behind them are
	// reported far more gently: declaring infrastructure forge never
	// generated is a SUPPORTED use of this file, not a mistake.
	var orphans []string
	for name := range declared {
		if !inCode[name] {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	for _, name := range orphans {
		findings = append(findings, componentDriftFinding{Name: name})
	}
	return findings, nil
}

// runComponentDrift is the text-mode arm. It prints findings and returns nil
// — this lint never gates.
func runComponentDrift(projectDir string, cfg *config.ProjectConfig) error {
	findings, err := collectComponentDrift(projectDir, cfg)
	if err != nil {
		return err
	}
	if len(findings) == 0 {
		return nil
	}
	for _, f := range findings {
		if !f.Undeclared {
			fmt.Printf("⚠️  %s declares %q, which no Go component matches.\n"+
				"    Fine if it is infrastructure forge did not generate (NATS, a cache, a sidecar).\n"+
				"    Otherwise delete the entry — the component it named is gone.\n",
				codegen.WorkloadsKCLRelPath, f.Name)
			continue
		}
		fmt.Printf("⚠️  %s %q has no entry in %s, so no environment deploys it.\n"+
			"    Add:\n\n%s\n",
			f.Kind, f.Name, codegen.WorkloadsKCLRelPath, f.Stanza)
	}
	return nil
}

// collectComponentDriftJSON is the JSON arm. Every finding is a warning and
// the step never gates, matching text mode.
func collectComponentDriftJSON(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
	findings, err := collectComponentDrift(rc.cwd, rc.cfg)
	if err != nil {
		return nil, false, err
	}
	out := make([]lintJSONFinding, 0, len(findings))
	for _, f := range findings {
		if !f.Undeclared {
			out = append(out, lintJSONFinding{
				File:     codegen.WorkloadsKCLRelPath,
				Severity: "warning",
				Rule:     "component-drift/orphan-entry",
				Message: fmt.Sprintf("%s declares %q, which no Go component matches",
					codegen.WorkloadsKCLRelPath, f.Name),
				FixHint: "expected if it is infrastructure forge did not generate; otherwise delete the entry",
			})
			continue
		}
		out = append(out, lintJSONFinding{
			File:     codegen.WorkloadsKCLRelPath,
			Severity: "warning",
			Rule:     "component-drift/undeclared-component",
			Message: fmt.Sprintf("%s %q has no entry in %s, so no environment deploys it",
				f.Kind, f.Name, codegen.WorkloadsKCLRelPath),
			FixHint: strings.TrimRight(f.Stanza, "\n"),
		})
	}
	return out, false, nil
}
