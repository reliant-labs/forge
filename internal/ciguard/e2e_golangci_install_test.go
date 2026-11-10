package ciguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// e2e-suite.yml's suite job used to install golangci-lint with
// golangci-lint's own install.sh. On 2026-11-09 that script's release
// download answered 504 and every nightly shard failed before running a
// test (forge #948). The job now installs it through install-release-tool
// (cached, retried, API fallback, sha256-checked); this test keeps it there.

var e2eSuiteWorkflow = filepath.Join("..", "..", ".github", "workflows", "e2e-suite.yml")

func TestE2ESuiteInstallsGolangciThroughReleaseTool(t *testing.T) {
	raw, err := os.ReadFile(e2eSuiteWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range suiteGolangciInstallProblems(t, raw) {
		t.Error(p)
	}
}

// suiteGolangciInstallProblems requires the suite job to install
// golangci-lint exactly once, through the local install-release-tool action,
// and never through install.sh. It returns what is wrong.
func suiteGolangciInstallProblems(t *testing.T, raw []byte) []string {
	t.Helper()
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string         `yaml:"name"`
				Uses string         `yaml:"uses"`
				Run  string         `yaml:"run"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", e2eSuiteWorkflow, err)
	}
	suite, ok := wf.Jobs["suite"]
	if !ok {
		t.Fatalf("%s: no suite job", e2eSuiteWorkflow)
	}
	var problems []string
	installs := 0
	for _, s := range suite.Steps {
		if strings.Contains(s.Run, "golangci-lint") && strings.Contains(s.Run, "install.sh") {
			problems = append(problems, fmt.Sprintf("suite step %q installs golangci-lint with install.sh; use "+
				"./.github/actions/install-release-tool (install.sh has no retry or fallback: forge #948)", s.Name))
		}
		if s.Uses == "./.github/actions/install-release-tool" && s.With["tool"] == "golangci-lint" {
			installs++
		}
	}
	if installs != 1 {
		problems = append(problems, fmt.Sprintf("suite job installs golangci-lint through "+
			"./.github/actions/install-release-tool %d times, want exactly 1", installs))
	}
	return problems
}

// The check rejects the previous install.sh step, so reverting the workflow
// fix turns this package red.
func TestSuiteGolangciInstallCheckRejectsInstallSh(t *testing.T) {
	const previous = `jobs:
  suite:
    steps:
      - name: Install golangci-lint
        run: |
          curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.14.0/install.sh \
            | sh -s -- -b "$(go env GOPATH)/bin" v2.14.0
          golangci-lint --version
`
	if len(suiteGolangciInstallProblems(t, []byte(previous))) == 0 {
		t.Fatal("the check accepted the install.sh step it exists to reject")
	}
}
