package ciguard

import (
	"os"
	"slices"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestWindowsChangedJobRunsOnRequestNotInTheMergeQueue holds the
// changed-packages Windows job (owner, 2026-11-13: four Windows-only reds
// reached main that day, each in a package its pull request changed) to its
// shape since it left the merge queue (2026-11-28: at 10-20 minutes it was the
// merge tier's critical path, and it failed 136 merge groups in 72 hours): it
// runs on a native Windows runner on a pull request labelled ci:windows and
// nowhere else, `CI gate` needs it (so a labelled pull request's red is red)
// but does not refuse a merge-queue run in which it skipped, it tests what
// scripts/ci-changed-packages.sh says the change reaches under -short, it
// needs no other job, and it is bounded at 20 minutes.
func TestWindowsChangedJobRunsOnRequestNotInTheMergeQueue(t *testing.T) {
	t.Parallel()
	const maxMinutes = 20
	raw, err := os.ReadFile(ciWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			If             string   `yaml:"if"`
			RunsOn         string   `yaml:"runs-on"`
			Needs          []string `yaml:"needs"`
			TimeoutMinutes int      `yaml:"timeout-minutes"`
			Steps          []struct {
				Name string            `yaml:"name"`
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	j, ok := wf.Jobs["windows-changed"]
	if !ok {
		t.Fatalf("%s has no windows-changed job; update this guard if it was renamed", ciWorkflow)
	}
	if strings.TrimSpace(j.If) != windowsLabelTier {
		t.Errorf("windows-changed runs if %q, want %q: never in the merge queue", j.If, windowsLabelTier)
	}
	if !strings.HasPrefix(j.RunsOn, "windows-") {
		t.Errorf("windows-changed runs on %q: only a native Windows run finds what it is for", j.RunsOn)
	}
	if len(j.Needs) != 0 {
		t.Errorf("windows-changed needs %v: it would start after them and lengthen the run", j.Needs)
	}
	if j.TimeoutMinutes <= 0 || j.TimeoutMinutes > maxMinutes {
		t.Errorf("windows-changed's timeout-minutes is %d; it must be set, between 1 and %d (owner, 2026-11-13)", j.TimeoutMinutes, maxMinutes)
	}
	var plans, tests, depth bool
	for _, s := range j.Steps {
		if strings.Contains(s.Run, "scripts/ci-changed-packages.sh HEAD^1") {
			plans = true
		}
		if strings.Contains(s.Run, "go test -short ") {
			tests = true
		}
		if strings.HasPrefix(s.Uses, "actions/checkout@") && s.With["fetch-depth"] == "2" {
			depth = true
		}
		if strings.HasPrefix(s.Uses, "actions/cache/save@") || strings.HasSuffix(s.Uses, "/save-go-caches") {
			t.Errorf("windows-changed saves a cache (%s): a pull request's cache is visible to no other run", s.Uses)
		}
	}
	if !plans {
		t.Error("windows-changed never runs scripts/ci-changed-packages.sh HEAD^1")
	}
	if !tests {
		t.Error("windows-changed never runs go test -short")
	}
	if !depth {
		t.Error("windows-changed does not check out with fetch-depth: 2; HEAD^1 would not resolve")
	}
	gate := wf.Jobs["gate"]
	if !slices.Contains(gate.Needs, "windows-changed") {
		t.Error("CI gate does not need windows-changed: a ci:windows pull request's Windows failure would not show in CI gate")
	}
	var every string
	for _, s := range gate.Steps {
		if s.Name == "Every job passed" {
			every = s.Run
		}
	}
	if every == "" {
		t.Fatal(`CI gate has no "Every job passed" step; update this guard if it was renamed`)
	}
	if strings.Contains(every, `.key == "windows-changed"`) {
		t.Error(`CI gate's "Every job passed" refuses a merge-queue run whose windows-changed job skipped: it no longer runs there, so every merge would fail`)
	}
}
