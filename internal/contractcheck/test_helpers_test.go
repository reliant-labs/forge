// File: internal/contractcheck/test_helpers_test.go
//
// Tiny shared helpers for the rule tests in this package. Mirrors the
// equivalents in internal/linter/forgeconv/test_helpers_test.go (and
// the bottom of forgeconv_test.go); kept local so contractcheck tests
// don't have to reach into another test-only package.

package contractcheck

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/linter/forgeconv"
)

func mkdirAll(dir string) error {
	return os.MkdirAll(filepath.Clean(dir), 0o755)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

// findingsForRule filters a finding slice to a single rule. Keeps tests
// focused — Inspect runs multiple rules in one pass when called with
// no Options.Rules filter, so a fixture could surface unrelated findings.
func findingsForRule(findings []forgeconv.Finding, rule string) []forgeconv.Finding {
	var out []forgeconv.Finding
	for _, f := range findings {
		if f.Rule == rule {
			out = append(out, f)
		}
	}
	return out
}
