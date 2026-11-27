package lint

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

const seoGoodPage = `<!doctype html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>%TITLE%</title>
<meta name="description" content="%TITLE%: small, reliable rockets for hobbyists and schools, with kits that ship in a week.">
<link rel="canonical" href="https://acme.example%PATH%">
<meta property="og:title" content="Acme Rockets"><meta property="og:description" content="Small, reliable rockets.">
<meta property="og:type" content="website"><meta property="og:url" content="https://acme.example%PATH%">
<meta property="og:image" content="https://acme.example/og.png"><meta name="twitter:card" content="summary_large_image">
</head><body><main><a href="/about">About the Acme team</a></main></body></html>`

func seoPage(title, path string) string {
	return strings.NewReplacer("%TITLE%", title, "%PATH%", path).Replace(seoGoodPage)
}

// seoTestProject writes a project with one built good site (web), one built
// bad site (bad), one unbuilt Vite site (draft), and a Next.js server app no
// env publishes statically (console).
func seoTestProject(t *testing.T) (string, *config.ProjectConfig, seoBindingsFunc) {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"frontends/web/dist/index.html":    seoPage("Acme Rockets — rockets for everyone", "/"),
		"frontends/web/dist/about.html":    seoPage("About Acme Rockets — the team", "/about"),
		"frontends/web/dist/404.html":      "<html><head><title>x</title><meta name=robots content=noindex></head></html>",
		"frontends/bad/out/index.html":     `<html><head><meta name="robots" content="noindex"><link rel="canonical" href="https://acme.example/"></head><body></body></html>`,
		"frontends/bad/out/pricing.html":   `<html lang="en"><head><title>Pricing for Acme rockets</title><link rel="canonical" href="https://elsewhere.example/pricing"></head></html>`,
		"frontends/draft/index.html":       `<!doctype html><html lang="en"><head><title>Vite + React</title></head><body><div id="root"></div></body></html>`,
		"frontends/console/next.config.ts": "export default {}",
	})
	cfg := loadTestConfig(t, staticExportForgeYAML)
	cfg.Frontends = []config.FrontendConfig{
		config.FrontendConfig{Name: "web", Type: "vite-spa"}.WithDir("frontends/web"),
		config.FrontendConfig{Name: "bad", Type: "nextjs", Output: "static"}.WithDir("frontends/bad"),
		config.FrontendConfig{Name: "draft", Type: "vite-spa"}.WithDir("frontends/draft"),
		config.FrontendConfig{Name: "console", Type: "nextjs", Output: "standalone"}.WithDir("frontends/console"),
	}
	bindings := func(context.Context, string, *config.ProjectConfig) map[string][]seoBinding {
		return map[string][]seoBinding{
			"web":   {{Env: "staging", PublicDir: "dist", SiteURL: "https://staging.acme.example"}, {Env: "prod", PublicDir: "dist", SiteURL: "https://acme.example"}},
			"bad":   {{Env: "prod", PublicDir: "out"}},
			"draft": {{Env: "prod", PublicDir: "dist"}},
		}
	}
	return root, cfg, bindings
}

func seoRules(fs []lintJSONFinding, sev string) []string {
	var out []string
	for _, f := range fs {
		if f.Rule != ruleRanNote && (sev == "" || f.Severity == sev) {
			out = append(out, f.Rule+" "+f.File)
		}
	}
	slices.Sort(out)
	return out
}

func seoNotes(fs []lintJSONFinding) string {
	var b strings.Builder
	for _, f := range fs {
		if f.Rule == ruleRanNote {
			b.WriteString(f.Message + "\n")
		}
	}
	return b.String()
}

func TestSEOLaneJudgesEachStaticSite(t *testing.T) {
	t.Parallel()
	root, cfg, bindings := seoTestProject(t)
	fs, err := seoFindings(context.Background(), root, cfg, bindings)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if strings.HasPrefix(f.File, "frontends/web/") {
			t.Errorf("the good site has a finding: %+v", f)
		}
	}
	errs := seoRules(fs, lintSevError)
	for _, want := range []string{
		"seo-title-missing frontends/bad/out/index.html",
		"seo-indexing-mismatch frontends/bad/out/index.html",
		"seo-canonical-foreign-host frontends/bad/out/pricing.html",
	} {
		if !slices.Contains(errs, want) {
			t.Errorf("error findings %v lack %q", errs, want)
		}
	}
	if !slices.Contains(seoRules(fs, ""), "seo-title-placeholder frontends/draft/index.html") {
		t.Errorf("the unbuilt Vite site's index.html was not judged: %v", seoRules(fs, ""))
	}
	notes := seoNotes(fs)
	for _, want := range []string{
		"web (vite-spa): judged 2 built page(s) in frontends/web/dist (env prod)",
		"draft (vite-spa): frontends/draft/dist is not built, so only the head frontends/draft/index.html declares was judged",
		"console (nextjs): not applicable — no env publishes it as a static site",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	for _, f := range fs {
		if f.Rule != ruleRanNote && f.FixHint == "" {
			t.Errorf("finding without a fix: %+v", f)
		}
	}
}

func TestSEOLaneGatesOnErrorsOnlyAndWarnsFirst(t *testing.T) {
	t.Parallel()
	root, cfg, bindings := seoTestProject(t)
	fs, err := seoFindings(context.Background(), root, cfg, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if !anyErrorFinding(fs) {
		t.Fatal("the bad site gated nothing")
	}

	// A project pinned to the release before the lane: its errors warn, with
	// the rollout note, and nothing gates.
	cfg.ForgeVersion = "v1.11.0"
	graded, gated := gradeProjectLane(cfg, fs, anyErrorFinding(fs))
	if gated || anyErrorFinding(graded) {
		t.Errorf("a v1.11.0 pin: gated=%v, errors=%v; want the new rules to warn", gated, seoRules(graded, lintSevError))
	}
	graced := 0
	for _, f := range graded {
		if f.Grace != nil {
			graced++
			if f.Grace.GatesFrom != "v1.13.0" {
				t.Errorf("%s gates from %q, want v1.13.0", f.Rule, f.Grace.GatesFrom)
			}
		}
	}
	if graced == 0 {
		t.Error("no finding carries the warn-first note")
	}

	// From the gating release on, an error gates.
	cfg.ForgeVersion = "v1.13.0"
	if _, gated := gradeProjectLane(cfg, fs, anyErrorFinding(fs)); !gated {
		t.Error("a v1.13.0 pin: the error findings did not gate")
	}
}

func TestSEOLaneNotApplicableWithoutAFrontend(t *testing.T) {
	t.Parallel()
	cfg := loadTestConfig(t, staticExportForgeYAML)
	called := false
	fs, err := seoFindings(context.Background(), t.TempDir(), cfg, func(context.Context, string, *config.ProjectConfig) map[string][]seoBinding {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("rendered the envs of a project with no frontend")
	}
	if len(fs) != 1 || fs[0].Rule != ruleRanNote || !strings.Contains(fs[0].Message, "no frontend") || anyErrorFinding(fs) {
		t.Errorf("findings = %+v", fs)
	}
}

func TestRenderedSEOFrontendsReadsSiteURL(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"output":{"frontends":[{"name":"web","runtime":{"type":"hosted"},"public_dir":"dist","base_path":"/docs",
		"env_vars":[{"name":"OTHER","value":"x"},{"name":"SITE_URL","value":"https://acme.example"}]}]}}`)
	got := renderedSEOFrontends(raw)
	if len(got) != 1 || got[0].siteURL != "https://acme.example" || got[0].basePath != "/docs" || got[0].publicDir != "dist" || got[0].runtime != "hosted" {
		t.Errorf("renderedSEOFrontends = %+v", got)
	}
}

// B1: a built site mounted under a base path is judged at its real URLs —
// the root and nested pages under /docs/, never /docs/docs/ — with the
// origin from SITE_URL or, without one, from the pages' canonicals.
func TestSEOLaneJudgesABasePathSiteAtItsRealURLs(t *testing.T) {
	t.Parallel()
	for name, siteURL := range map[string]string{"SITE_URL": "https://acme.example", "canonical": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeTree(t, root, map[string]string{
				"frontends/docs/dist/index.html":         seoPage("Acme docs — rockets explained", "/docs/"),
				"frontends/docs/dist/guide/index.html":   seoPage("The Acme rocket guide", "/docs/guide/"),
				"frontends/docs/dist/guide/install.html": seoPage("Installing an Acme launch kit", "/docs/guide/install"),
				// A planted finding on a nested page, to read its route.
				"frontends/docs/dist/guide/faq.html": strings.Replace(seoPage("Acme rocket questions", "/docs/guide/faq"), `<meta name="viewport" content="width=device-width, initial-scale=1">`, "", 1),
			})
			cfg := loadTestConfig(t, staticExportForgeYAML)
			cfg.Frontends = []config.FrontendConfig{config.FrontendConfig{Name: "docs", Type: "vite-spa"}.WithDir("frontends/docs")}
			bindings := func(context.Context, string, *config.ProjectConfig) map[string][]seoBinding {
				return map[string][]seoBinding{"docs": {{Env: "prod", PublicDir: "dist", BasePath: "/docs", SiteURL: siteURL}}}
			}
			fs, err := seoFindings(context.Background(), root, cfg, bindings)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, f := range fs {
				if f.Rule == ruleRanNote {
					continue
				}
				got = append(got, f.Rule+" "+f.Message)
				if strings.Contains(f.Message, "/docs/docs") || strings.HasPrefix(f.Rule, "seo-canonical-") {
					t.Errorf("judged at the wrong URL: %s %s", f.Rule, f.Message)
				}
			}
			if len(got) != 1 || !strings.HasPrefix(got[0], "seo-viewport-missing /docs/guide/faq:") {
				t.Errorf("findings = %v, want only the planted viewport finding at /docs/guide/faq", got)
			}
		})
	}
}

// B1, source side: an unbuilt Next.js site under a base path resolves its
// relative metadata URLs against the site's real URL, never against the
// placeholder its pages are first read under.
func TestSEOLaneResolvesRelativeMetadataAgainstTheRealSite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"frontends/docs/app/layout.tsx": `export const metadata = {
  title: "Acme docs — rockets explained",
  description: "Everything about building, launching and recovering an Acme rocket, step by step.",
  alternates: { canonical: "/docs/" },
  openGraph: { title: "Acme docs", description: "Rockets explained.", type: "website", url: "/docs/", images: ["/docs/og.png"] },
  twitter: { card: "summary_large_image" },
};
export default function L({children}) { return <html lang="en"><body>{children}</body></html>; }`,
	})
	cfg := loadTestConfig(t, staticExportForgeYAML)
	cfg.Frontends = []config.FrontendConfig{config.FrontendConfig{Name: "docs", Type: "nextjs", Output: "static"}.WithDir("frontends/docs")}
	bindings := func(context.Context, string, *config.ProjectConfig) map[string][]seoBinding {
		return map[string][]seoBinding{"docs": {{Env: "prod", PublicDir: "out", BasePath: "/docs", SiteURL: "https://acme.example"}}}
	}
	fs, err := seoFindings(context.Background(), root, cfg, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if got := seoRules(fs, ""); len(got) != 0 {
		t.Errorf("findings = %v, want none (the canonical and og:url resolve to https://acme.example/docs/)", got)
	}
}

func TestSEOLaneWarnsOnAnAmbiguousLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"frontends/web/app/layout.tsx": `export const metadata = { title: "Acme Rockets — rockets for everyone" }; export default function L({children}) { return <html lang="en">{children}</html>; }`,
		"frontends/web/app/layout.js":  `export default function L({children}) { return <html lang="en">{children}</html>; }`,
	})
	cfg := loadTestConfig(t, staticExportForgeYAML)
	cfg.Frontends = []config.FrontendConfig{config.FrontendConfig{Name: "web", Type: "nextjs", Output: "static"}.WithDir("frontends/web")}
	bindings := func(context.Context, string, *config.ProjectConfig) map[string][]seoBinding {
		return map[string][]seoBinding{"web": {{Env: "prod", PublicDir: "out", SiteURL: "https://acme.example"}}}
	}
	fs, err := seoFindings(context.Background(), root, cfg, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if anyErrorFinding(fs) {
		t.Errorf("an ambiguous layout gated: %v", seoRules(fs, lintSevError))
	}
	warned := false
	for _, f := range fs {
		if f.Rule == "skipped" && f.Severity == lintSevWarning && strings.Contains(f.Message, "layout.tsx and frontends/web/app/layout.js") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning names both layout files: %+v", fs)
	}
}
