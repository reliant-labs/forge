package lint

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

// Graph findings answer to forge.yaml's lint.rules like every forge rule, so
// a project declaring its roots one at a time can hold the unreachable rule
// at warn until the last root lands.
func TestRegradeGraphFindings_LintRulesSetTheSeverity(t *testing.T) {
	findings := func() []codegen.GraphFinding {
		return []codegen.GraphFinding{
			{Rule: codegen.GraphRuleUnreachable, Error: true, File: "internal/handlers/alpha", Message: "unreached"},
			{Rule: codegen.GraphRuleAudience, Error: true, File: "internal/app/providers.go", Message: "audience"},
			{Rule: codegen.GraphRuleUnreachable, Error: false, File: "internal/blobstore", Message: "pending"},
		}
	}
	grade := func(rules map[string]string) string {
		got := regradeGraphFindings(findings(), config.LintConfig{Rules: rules}.EffectiveRules())
		var out []string
		for _, f := range got {
			sev := "warn"
			if f.Error {
				sev = "error"
			}
			out = append(out, f.Message+"/"+sev)
		}
		return strings.Join(out, ", ")
	}
	cases := []struct {
		name  string
		rules map[string]string
		want  string
	}{
		{"default: the analyzer's severity", nil, "unreached/error, audience/error, pending/warn"},
		{"warn stops the gate", map[string]string{"forge-graph-unreachable": "warn"}, "unreached/warn, audience/error, pending/warn"},
		{"rule ids are case-insensitive", map[string]string{"Forge-Graph-Unreachable": "warn"}, "unreached/warn, audience/error, pending/warn"},
		{"off drops the finding", map[string]string{"forge-graph-unreachable": "off"}, "audience/error"},
		{"error raises a warning", map[string]string{"forge-graph-unreachable": "error"}, "unreached/error, audience/error, pending/error"},
		{"an exact id beats the wildcard", map[string]string{"*": "warn", "forge-graph-audience": "error"}, "unreached/warn, audience/error, pending/warn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grade(tc.rules); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}

	var b strings.Builder
	if formatGraphFindings(&b, regradeGraphFindings(findings()[:1], config.LintConfig{Rules: map[string]string{"forge-graph-unreachable": "warn"}}.EffectiveRules())) {
		t.Fatalf("a finding re-graded to warn must not fail the lane:\n%s", b.String())
	}
}
