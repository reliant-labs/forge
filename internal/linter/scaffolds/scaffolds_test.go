package scaffolds

import (
	"path/filepath"
	"testing"
)

func TestLintRoot_Clean(t *testing.T) {
	t.Parallel()
	res, err := LintRoot(filepath.Join("testdata", "clean"))
	if err != nil {
		t.Fatalf("LintRoot returned error: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected zero findings on clean fixture, got %d: %+v", len(res.Findings), res.Findings)
	}
	if res.HasErrors() {
		t.Fatal("clean fixture must not produce errors")
	}
}

func TestLintRoot_ScaffoldMarkerPresent(t *testing.T) {
	t.Parallel()
	res, err := LintRoot(filepath.Join("testdata", "scaffold_marker_present"))
	if err != nil {
		t.Fatalf("LintRoot returned error: %v", err)
	}
	if !res.HasErrors() {
		t.Fatal("expected scaffold-not-customized error, got none")
	}
	if !findingMatches(res.Findings, "scaffold-not-customized") {
		t.Fatalf("expected a scaffold-not-customized finding, got: %+v", res.Findings)
	}
}

func TestLintRoot_GenMissingHeader(t *testing.T) {
	t.Parallel()
	res, err := LintRoot(filepath.Join("testdata", "gen_missing_header"))
	if err != nil {
		t.Fatalf("LintRoot returned error: %v", err)
	}
	if !findingMatches(res.Findings, "gen-missing-header") {
		t.Fatalf("expected a gen-missing-header finding, got: %+v", res.Findings)
	}
	if !res.HasErrors() {
		t.Fatal("missing canonical header is an error-severity finding")
	}
}

func TestLintRoot_GenMissingSource(t *testing.T) {
	t.Parallel()
	res, err := LintRoot(filepath.Join("testdata", "gen_missing_source")) //nolint:staticcheck // testdata path
	if err != nil {
		t.Fatalf("LintRoot returned error: %v", err)
	}
	if !findingMatches(res.Findings, "gen-missing-source") {
		t.Fatalf("expected a gen-missing-source finding, got: %+v", res.Findings)
	}
	// Source missing is a warning, not an error: the file is still
	// recognisable as forge-owned thanks to the canonical header.
	if res.HasErrors() {
		t.Fatal("missing Source: line should be a warning, not an error")
	}
}

func TestIsGenFilename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{"handlers/api/handlers_crud_gen.go", true},
		{"handlers/api/handlers_crud_gen_test.go", true},
		{"handlers/api/authorizer_gen.go", true},
		{"handlers/api/service.go", false},
		{"handlers/api/handlers.go", false},
		{"pkg/middleware/auth_gen.go", true},
		{"pkg/middleware/auth.go", false},
	}
	for _, c := range cases {
		if got := isGenFilename(c.path); got != c.want {
			t.Errorf("isGenFilename(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func findingMatches(findings []Finding, rule string) bool {
	for _, f := range findings {
		if f.Rule == rule {
			return true
		}
	}
	return false
}
