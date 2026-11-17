package lint

import (
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/suppress"
	"github.com/reliant-labs/forge/pkg/dbanalyze"
)

func TestGradeFindingWarnsFirstAndHonoursLintRules(t *testing.T) {
	rule := dbanalyze.RuleTableRewrite
	ro := lintRollouts[rule]
	if ro.GatesFrom == "" {
		t.Fatalf("%s has no warn-first rollout", rule)
	}

	behind := &config.ProjectConfig{ModulePath: "example.com/app", ForgeVersion: ro.IntroducedIn}
	sev, keep, note := GradeFinding(behind, rule, lintSevError)
	if sev != lintSevWarning || !keep || note == "" {
		t.Errorf("pinned at %s: %q %v %q, want a warning with a note", ro.IntroducedIn, sev, keep, note)
	}

	at := &config.ProjectConfig{ModulePath: "example.com/app", ForgeVersion: ro.GatesFrom}
	if sev, keep, note := GradeFinding(at, rule, lintSevError); sev != lintSevError || !keep || note != "" {
		t.Errorf("pinned at %s: %q %v %q, want an error", ro.GatesFrom, sev, keep, note)
	}

	off := &config.ProjectConfig{ModulePath: "example.com/app", ForgeVersion: ro.GatesFrom,
		Lint: config.LintConfig{Rules: map[string]string{rule: suppress.SeverityOff}}}
	if _, keep, _ := GradeFinding(off, rule, lintSevError); keep {
		t.Error("lint.rules {rule: off} kept the finding")
	}
}

// v1.7.0 shipped the migration pass alone: the query rules warn from the
// release that ships them, not from a gate date chosen before they existed.
func TestDBAnalyzeQueryRulesWarnFromTheirOwnRelease(t *testing.T) {
	for _, rule := range dbanalyze.MigrationRuleIDs() {
		if ro := lintRollouts[rule]; ro.IntroducedIn != "v1.7.0" || ro.GatesFrom != "v1.7.1" {
			t.Errorf("%s: %s → %s, want v1.7.0 → v1.7.1", rule, ro.IntroducedIn, ro.GatesFrom)
		}
	}
	for _, rule := range dbanalyze.QueryRuleIDs() {
		if ro := lintRollouts[rule]; ro.IntroducedIn != "v1.8.0" || ro.GatesFrom != "v1.8.1" {
			t.Errorf("%s: %s → %s, want v1.8.0 → v1.8.1", rule, ro.IntroducedIn, ro.GatesFrom)
		}
	}
}
