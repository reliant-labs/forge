package ciguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestE2ESuiteInstallsGolangciThroughReleaseTool pins how the e2e suite's
// shards install golangci-lint: once, through the local install-release-tool
// action (cached, retried, API fallback, sha256-checked), never by piping
// golangci-lint's install.sh into sh. On 2026-11-09 install.sh's release
// download answered 504 and all four nightly shards failed before running a
// test. The version pin itself is TestReleaseToolPinsMatchForge's.
func TestE2ESuiteInstallsGolangciThroughReleaseTool(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "workflows", "e2e-suite.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string            `yaml:"name"`
				Uses string            `yaml:"uses"`
				With map[string]string `yaml:"with"`
				Run  string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
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
			t.Errorf("%s suite step %q installs golangci-lint through install.sh; use ./.github/actions/install-release-tool", path, s.Name)
		}
	}
	if installs != 1 {
		t.Errorf("%s suite job installs golangci-lint through install-release-tool %d times, want exactly 1", path, installs)
	}
}
