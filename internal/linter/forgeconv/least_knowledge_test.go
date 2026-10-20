package forgeconv

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/depsurface"
)

func lkFixture() *depsurface.Package {
	wide := make([]string, 12)
	for i := range wide {
		wide[i] = string(rune('A' + i))
	}
	return &depsurface.Package{
		Dir: "internal/handlers/tools",
		Fields: []depsurface.Field{
			{Name: "Repo", File: "internal/handlers/tools/service.go", Line: 10, Type: "db.Repository", Interface: true, Foreign: true, Methods: wide, Used: map[string]bool{"A": true, "B": true}},
			{Name: "Clock", File: "internal/handlers/tools/service.go", Line: 11, Type: "Clock", Interface: true, Methods: []string{"Now", "Since"}, Used: map[string]bool{"Now": true}},
			{Name: "Small", File: "internal/handlers/tools/service.go", Line: 12, Type: "events.Publisher", Interface: true, Foreign: true, Methods: []string{"Publish", "Close"}, Used: map[string]bool{"Publish": true}},
			{Name: "Passed", File: "internal/handlers/tools/service.go", Line: 13, Type: "db.Repository", Interface: true, Foreign: true, Methods: wide, Used: map[string]bool{"A": true}, Escapes: true},
			{Name: "Bag", File: "internal/handlers/tools/service.go", Line: 14, Type: "any", Interface: true, Container: "`any`", Used: map[string]bool{}},
		},
		Downcasts: []depsurface.Downcast{{Field: "Repo", File: "internal/handlers/tools/raw.go", Line: 3, Target: "*db.Repo"}},
	}
}

func lkSummary(fs []Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule+":"+string(f.Severity)+"@"+f.File)
	}
	return strings.Join(out, " ")
}

func TestLeastKnowledge_WarningsByDefault(t *testing.T) {
	fs := leastKnowledgeFindings(lkFixture(), false)
	for _, f := range fs {
		if f.Severity != SeverityWarning {
			t.Errorf("%s is %s by default; the least-knowledge rules land as warnings", f.Rule, f.Severity)
		}
	}
	// Repo, Clock and Small are wider than their use; Passed escapes, so its
	// calls here are not the whole story; Bag is a container.
	if got := len(fs); got != 5 {
		t.Fatalf("%d findings: %s", got, lkSummary(fs))
	}
	if !strings.Contains(fs[0].Remediation, "forge fix narrow-deps internal/handlers/tools") {
		t.Errorf("a foreign field's fix is narrow-deps: %s", fs[0].Remediation)
	}
}

func TestLeastKnowledge_StrictAppliesTheDesignSeverities(t *testing.T) {
	sev := map[string]Severity{}
	for _, f := range leastKnowledgeFindings(lkFixture(), true) {
		key := f.Rule
		if f.Rule == RuleDepsSurface {
			key += "/" + strings.Fields(f.Message)[0]
		}
		sev[key] = f.Severity
	}
	want := map[string]Severity{
		RuleDepsSurface + "/Deps.Repo":  SeverityError,   // foreign, 12 methods, unused ones
		RuleDepsSurface + "/Deps.Clock": SeverityWarning, // declared here
		RuleDepsSurface + "/Deps.Small": SeverityWarning, // foreign but narrow
		RuleNoContainerDeps:             SeverityError,
		RuleDepsNoDowncast:              SeverityError,
	}
	for k, w := range want {
		if sev[k] != w {
			t.Errorf("%s = %q, want %q (all: %v)", k, sev[k], w, sev)
		}
	}
}
