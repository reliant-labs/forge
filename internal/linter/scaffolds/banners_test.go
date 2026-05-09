package scaffolds

import (
	"path/filepath"
	"testing"
)

func TestBannerLintRoot_MissingTier1(t *testing.T) {
	t.Parallel()
	res, err := BannerLintRoot(filepath.Join("testdata", "banners", "missing_tier1"))
	if err != nil {
		t.Fatalf("BannerLintRoot returned error: %v", err)
	}
	if !findingMatches(res.Findings, "banner-tier1-missing-generated-header") {
		t.Fatalf("expected a banner-tier1-missing-generated-header finding, got: %+v", res.Findings)
	}
}

func TestBannerLintRoot_MissingTier2(t *testing.T) {
	t.Parallel()
	res, err := BannerLintRoot(filepath.Join("testdata", "banners", "missing_tier2"))
	if err != nil {
		t.Fatalf("BannerLintRoot returned error: %v", err)
	}
	if !findingMatches(res.Findings, "banner-tier2-missing-scaffold-header") {
		t.Fatalf("expected a banner-tier2-missing-scaffold-header finding, got: %+v", res.Findings)
	}
}

func TestBannerLintRoot_Correct(t *testing.T) {
	t.Parallel()
	res, err := BannerLintRoot(filepath.Join("testdata", "banners", "correct"))
	if err != nil {
		t.Fatalf("BannerLintRoot returned error: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected zero findings on correct fixture, got %d: %+v", len(res.Findings), res.Findings)
	}
}

func TestClassifyTemplate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rel  string
		want templateTier
	}{
		{"internal/templates/service/handlers_gen.go.tmpl", tier1Generated},
		{"internal/templates/middleware/auth_gen.go.tmpl", tier1Generated},
		{"internal/templates/frontend/hooks.ts.tmpl", tier1Generated},
		{"internal/templates/ci/github/ci.yml.tmpl", tier1Generated},
		{"internal/templates/internal-package/contract.go.tmpl", tier2Scaffold},
		{"internal/templates/frontend/pages/list-page.tsx.tmpl", tier2Scaffold},
		{"internal/packs/jwt-auth/templates/jwt_validator.go.tmpl", tier2Scaffold},
		{"internal/templates/project/setup.go.tmpl", tier3UserOwned},
		{"internal/templates/service/service.go.tmpl", tier3UserOwned},
		{"internal/templates/worker/worker.go.tmpl", tier3UserOwned},
		{"internal/templates/project/Makefile.tmpl", tierSkip},
		{"internal/templates/project/go.mod.tmpl", tierSkip},
		{"internal/templates/project/Dockerfile.tmpl", tierSkip},
	}
	for _, c := range cases {
		got := classifyTemplate(c.rel)
		if got != c.want {
			t.Errorf("classifyTemplate(%q) = %d, want %d", c.rel, got, c.want)
		}
	}
}
