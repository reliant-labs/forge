// least_knowledge.go — the composition graph's least-knowledge rules
// (docs/design/composition-graph.md §2, K1/K2/P1), over every component
// package's Deps as internal/depsurface reads it:
//
//	forgeconv-deps-surface      an interface Deps field declares methods the
//	                            package never calls through it (K1)
//	forgeconv-no-container-deps a Deps field holds a container: the
//	                            composition root's types, the command
//	                            tree's Deps, a locator, `any` (K2)
//	forgeconv-deps-no-downcast  a type assertion turns a Deps field back into
//	                            a concrete type (P1)
//
// They land as WARNINGS. Under `forge lint --strict` they take the severity
// the design settles on (§9.2): deps-surface is an error for a FOREIGN
// interface (declared in another package) of more than ten methods with
// unused ones, and a warning otherwise; the other two are errors.

package forgeconv

import (
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/internal/depsurface"
)

// Rule ids.
const (
	RuleDepsSurface     = "forgeconv-deps-surface"
	RuleNoContainerDeps = "forgeconv-no-container-deps"
	RuleDepsNoDowncast  = "forgeconv-deps-no-downcast"
)

// depsSurfaceWideMethods is the foreign interface size past which an unused
// method is an error under --strict (§9.2).
const depsSurfaceWideMethods = 10

// LintLeastKnowledge runs the three least-knowledge rules over the component
// packages under root/internal.
func LintLeastKnowledge(root string, opts LintOptions) (Result, error) {
	dirs, err := depsurface.FindComponentDirs(root)
	if err != nil {
		return Result{}, err
	}
	pkgs, err := depsurface.Analyze(root, dirs)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, p := range pkgs {
		res.Findings = append(res.Findings, leastKnowledgeFindings(p, opts.Strict)...)
	}
	return res, nil
}

func leastKnowledgeFindings(p *depsurface.Package, strict bool) []Finding {
	sev := func(gates bool) Severity {
		if strict && gates {
			return SeverityError
		}
		return SeverityWarning
	}
	var out []Finding
	for _, f := range p.Fields {
		if f.Container != "" {
			out = append(out, Finding{
				Rule: RuleNoContainerDeps, Severity: sev(true), File: f.File, Line: f.Line,
				Message:     fmt.Sprintf("Deps.%s (%s) is %s — a component receives the capabilities it uses, never a container to fetch them from", f.Name, f.Type, f.Container),
				Remediation: "take each collaborator as its own typed field (an interface declared here, holding the methods the package calls), and configuration as the component's own config block; internal/app/compose.go fills each by type",
			})
		}
		unused := depsurface.Unused(f)
		if !f.Interface || len(unused) == 0 || f.Escapes || f.Downcast {
			continue
		}
		wide := f.Foreign && len(f.Methods) > depsSurfaceWideMethods
		where := "declared in this package"
		fix := fmt.Sprintf("delete the methods this package never calls from %s", f.Type)
		if f.Foreign {
			where = "declared in another package"
			fix = fmt.Sprintf("`forge fix narrow-deps %s` declares the %d-method interface the package calls, here, and retypes the field (compose.go needs no change)", p.Dir, len(f.Used))
		}
		msg := fmt.Sprintf("Deps.%s (%s, %d methods, %s) — the package calls %d of them through it; unused: %s", f.Name, f.Type, len(f.Methods), where, len(f.Used), summarize(unused, 6))
		if len(f.Used) == 0 {
			msg = fmt.Sprintf("Deps.%s (%s) is never called through — the package does not need it", f.Name, f.Type)
			fix = "delete the field (and its key in internal/app/compose.go)"
		}
		out = append(out, Finding{Rule: RuleDepsSurface, Severity: sev(wide), File: f.File, Line: f.Line, Message: msg, Remediation: fix})
	}
	for _, d := range p.Downcasts {
		out = append(out, Finding{
			Rule: RuleDepsNoDowncast, Severity: sev(true), File: d.File, Line: d.Line,
			Message:     fmt.Sprintf("a type assertion turns Deps.%s back into %s — the field's interface is the capability the component was granted, and the assertion takes the rest", d.Field, d.Target),
			Remediation: fmt.Sprintf("declare the methods of %s the package needs on the field's interface (or a second field), so the composition grants them explicitly", d.Target),
		})
	}
	return out
}

func summarize(names []string, limit int) string {
	if len(names) <= limit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:limit], ", "), len(names)-limit)
}
