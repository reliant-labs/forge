package ciguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestE2ESuiteInstallsGolangciThroughReleaseTool holds e2e-suite.yml's
// `suite` job to installing golangci-lint through install-release-tool
// (cached, retried, API fallback, sha256-checked), never golangci-lint's
// install.sh: on 2026-11-09 that script's release download answered 504 and
// all four nightly shards failed before running a test (forge #948). The pin
// tests accept either spelling, so without this one a revert stays green.
func TestE2ESuiteInstallsGolangciThroughReleaseTool(t *testing.T) {
	path := filepath.Join(workflowsDir, "e2e-suite.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
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
	suite, ok := wf.Jobs["suite"]
	if !ok {
		t.Fatalf("%s has no suite job", path)
	}
	installs := 0
	for _, s := range suite.Steps {
		if s.Uses == "./.github/actions/install-release-tool" && s.With["tool"] == "golangci-lint" {
			installs++
		}
		if strings.Contains(s.Run, "golangci-lint") && strings.Contains(s.Run, "install.sh") {
			t.Errorf("suite step %q installs golangci-lint with install.sh; use ./.github/actions/install-release-tool (tool: golangci-lint)", s.Name)
		}
	}
	if installs != 1 {
		t.Errorf("suite installs golangci-lint through ./.github/actions/install-release-tool %d times, want exactly 1", installs)
	}
}
