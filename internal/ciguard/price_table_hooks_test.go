package ciguard_test

import (
	"os"
	"regexp"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Read the real configuration: a formatter exemption must not become a
// validation or secret-scanning exemption for the vendored price table.
func TestPriceTablePreCommitBoundary(t *testing.T) {
	data, err := os.ReadFile("../../.pre-commit-config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	type hook struct {
		ID           string   `yaml:"id"`
		Files        string   `yaml:"files"`
		Exclude      string   `yaml:"exclude"`
		TypesExclude []string `yaml:"exclude_types"`
	}
	var config struct {
		Files   string `yaml:"files"`
		Exclude string `yaml:"exclude"`
		Repos   []struct {
			Hooks []hook `yaml:"hooks"`
		} `yaml:"repos"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	matches := func(pattern, path string) bool {
		t.Helper()
		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Fatal(err)
		}
		return re.MatchString(path)
	}
	included := func(files, exclude, path string) bool {
		return (files == "" || matches(files, path)) && (exclude == "" || !matches(exclude, path))
	}
	const vendored = "pkg/llmprice/litellm_prices.json"
	paths := []string{
		vendored,
		"pkg/llmprice/other.json",
		"pkg/llmprice/litellm_prices.json.bak.json",
		"pkg/llmprice/litellm_pricesXjson",
		"pkg/llmprice/sub/litellm_prices.json",
		"other/pkg/llmprice/litellm_prices.json",
		"pkg/other/litellm_prices.json",
	}
	found := map[string]bool{}
	for _, repo := range config.Repos {
		for _, h := range repo.Hooks {
			if h.ID != "prettier" && h.ID != "check-json" && h.ID != "gitleaks" {
				continue
			}
			found[h.ID] = true
			if len(h.TypesExclude) != 0 {
				t.Errorf("%s unexpectedly excludes file types: %v", h.ID, h.TypesExclude)
			}
			for _, path := range paths {
				got := included(config.Files, config.Exclude, path) && included(h.Files, h.Exclude, path)
				// The non-JSON lookalike checks only the exclusion's exactness.
				if path == "pkg/llmprice/litellm_pricesXjson" {
					if h.Exclude != "" && matches(h.Exclude, path) {
						t.Errorf("%s excludes lookalike %s", h.ID, path)
					}
					continue
				}
				want := h.ID != "prettier" || path != vendored
				if got != want {
					t.Errorf("%s includes %s = %v, want %v", h.ID, path, got, want)
				}
			}
		}
	}
	for _, id := range []string{"prettier", "check-json", "gitleaks"} {
		if !found[id] {
			t.Errorf("missing %s hook", id)
		}
	}
}
