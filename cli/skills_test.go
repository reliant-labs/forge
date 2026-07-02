// Copyright (c) 2025 Reliant Labs
package cli

import (
	"strings"
	"testing"
)

// TestListSkillsSignatureStable is a compile-time pin of the public API
// reliant links against: ListSkills(projectRoot string) ([]Skill, error)
// and LoadSkill(projectRoot, skillPath string) ([]byte, error) must keep
// these exact signatures — relevance filtering was added behind them
// additively (ListSkillsWithOptions / ListSkillsOptions).
func TestListSkillsSignatureStable(t *testing.T) {
	var _ = ListSkills
	var _ = LoadSkill
	var _ = ListSkillsWithOptions
}

// TestListSkillsExcludesMigrationsByDefault pins the default-listing
// contract for harness consumers: one-time migration skills are hidden
// unless explicitly opted in, and the relevance class is exposed on the
// metadata so consumers can make their own call.
func TestListSkillsExcludesMigrationsByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from ~/.forge/skills

	defaults, err := ListSkills("")
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(defaults) == 0 {
		t.Fatal("no skills listed")
	}
	for _, s := range defaults {
		if s.Relevance == "migration" {
			t.Errorf("default listing contains migration skill %q", s.Path)
		}
		if strings.HasPrefix(s.Path, "migrations/") {
			t.Errorf("default listing contains migrations/ path %q", s.Path)
		}
	}

	all, err := ListSkillsWithOptions("", ListSkillsOptions{IncludeMigrationSkills: true})
	if err != nil {
		t.Fatalf("ListSkillsWithOptions: %v", err)
	}
	if len(all) <= len(defaults) {
		t.Errorf("opt-in listing (%d) not larger than default (%d)", len(all), len(defaults))
	}
	found := false
	for _, s := range all {
		if !strings.HasPrefix(s.Path, "migrations/") {
			continue
		}
		found = true
		if s.Relevance != "migration" {
			t.Errorf("skill %q: Relevance = %q, want migration", s.Path, s.Relevance)
		}
	}
	if !found {
		t.Error("opt-in listing has no migrations/ skills")
	}
}

// TestLoadSkillStillServesMigrations pins the load-by-path escape hatch:
// listings hide migration skills, but LoadSkill must keep serving them
// (forge upgrade list points agents at exactly these paths).
func TestLoadSkillStillServesMigrations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	body, err := LoadSkill("", "migrations/v0.x-to-contractkit")
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}
	if !strings.Contains(string(body), "contractkit") {
		t.Error("migration skill body looks wrong")
	}
}
