package seo

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/siteaudit"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func codes(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Code+"@"+f.Route)
	}
	slices.Sort(out)
	return out
}

func TestRuleIDsAreTheRawHTMLRules(t *testing.T) {
	t.Parallel()
	ids := RuleIDs()
	if len(ids) == 0 {
		t.Fatal("no rule IDs")
	}
	for _, want := range []string{"seo-title-missing", "seo-canonical-foreign-host", "seo-indexing-mismatch", "seo-og-tag-missing"} {
		if !slices.Contains(ids, want) {
			t.Errorf("RuleIDs lacks %s", want)
		}
	}
	for _, r := range siteaudit.Catalog() {
		if (r.Source == siteaudit.SourceSite || r.Source == siteaudit.SourceAxe) && slices.Contains(ids, RuleID(r.Code)) {
			t.Errorf("%s needs facts the raw HTML does not carry, so the lane cannot emit it", r.Code)
		}
	}
}

// TestRuleIDsCoverTheRawHTMLRules: every rule EvaluateRaw can report has a
// rule ID, so a rule siteaudit adds is a deliberate addition here (with a
// rollout row), never a finding lint.rules cannot grade.
func TestRuleIDsCoverTheRawHTMLRules(t *testing.T) {
	t.Parallel()
	for _, r := range siteaudit.Catalog() {
		raw := r.Source == siteaudit.SourceRaw || r.Source == siteaudit.SourceDOM
		_, mapped := ruleIDs[r.Code]
		switch {
		case raw && !mapped:
			t.Errorf("siteaudit rule %s runs in EvaluateRaw but has no rule ID: add %q to ruleIDs and give it a rollout row", r.Code, RuleID(r.Code))
		case !raw && mapped:
			t.Errorf("%s is a %s rule the raw HTML cannot judge; remove it from ruleIDs", r.Code, r.Source)
		}
	}
	for code, id := range ruleIDs {
		if id != RulePrefix+strings.ReplaceAll(code, "_", "-") {
			t.Errorf("%s: rule ID %q, want %q", code, id, RulePrefix+strings.ReplaceAll(code, "_", "-"))
		}
		if _, ok := siteaudit.LookupRule(code); !ok {
			t.Errorf("ruleIDs names %s, which siteaudit does not have", code)
		}
	}
}

func TestBuiltPagesServeEachFileAtItsURL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"index.html":              "<html></html>",
		"about.html":              "<html></html>",
		"docs/index.html":         "<html></html>",
		"blog/post.html":          "<html></html>",
		"404.html":                "<html></html>",
		"_not-found.html":         "<html></html>",
		"google0123abcd.html":     "google-site-verification",
		"assets/app.js":           "",
		".well-known/x/page.html": "<html></html>",
	})
	pages, truncated, err := BuiltPages(dir, "web/dist", "https://acme.example/")
	if err != nil || truncated {
		t.Fatalf("err %v truncated %v", err, truncated)
	}
	got := map[string]string{}
	for _, p := range pages {
		got[p.File] = p.URL
	}
	want := map[string]string{
		"web/dist/index.html":      "https://acme.example/",
		"web/dist/about.html":      "https://acme.example/about",
		"web/dist/docs/index.html": "https://acme.example/docs/",
		"web/dist/blog/post.html":  "https://acme.example/blog/post",
	}
	if len(got) != len(want) {
		t.Errorf("pages = %v, want %v", got, want)
	}
	for f, u := range want {
		if got[f] != u {
			t.Errorf("%s served at %q, want %q", f, got[f], u)
		}
	}
}

const literalLayout = `import type { Metadata } from "next";

export const metadata: Metadata = {
  metadataBase: new URL("https://acme.example"),
  title: { default: "Acme Rockets — rockets for everyone", template: "%s | Acme" },
  description: "Acme builds small, reliable rockets for hobbyists and schools, with kits that ship in a week.",
  alternates: { canonical: "/" },
  openGraph: {
    title: "Acme Rockets",
    description: "Small, reliable rockets.",
    url: "/",
    type: "website",
    images: [{ url: "/og.png", width: 1200, height: 630 }],
  },
  twitter: { card: "summary_large_image" },
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
`

func TestNextLayoutRendersTheHeadItsMetadataEmits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, map[string]string{"src/app/layout.tsx": literalLayout})
	p, ok := NextLayout(dir, "web", "https://acme.example/")
	if !ok {
		t.Fatal("no layout found")
	}
	if p.File != "web/src/app/layout.tsx" {
		t.Errorf("File = %q", p.File)
	}
	h := siteaudit.ExtractHead(p.HTML)
	if h.Title != "Acme Rockets — rockets for everyone" || h.Canonical != "https://acme.example/" || h.Lang != "en" ||
		h.Social["og:image"] != "https://acme.example/og.png" || h.Social["twitter:card"] != "summary_large_image" {
		t.Errorf("head = %+v\n%s", h, p.HTML)
	}
	if fs := Judge("https://acme.example/", []Page{p}); len(fs) != 0 {
		t.Errorf("a complete literal head: findings %v", codes(fs))
	}
}

func TestNextLayoutNeverJudgesAValueItDidNotSee(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, map[string]string{"app/layout.tsx": `
const siteName = process.env.SITE_NAME;
export const metadata = {
  title: siteName,
  description: ` + "`${siteName} is great`" + `,
  alternates: { canonical: process.env.SITE_URL },
  robots: { index: false },
};
export default function L({children}) { return <html>{children}</html>; }
`})
	p, ok := NextLayout(dir, "web", "https://acme.example/")
	if !ok {
		t.Fatal("no layout found")
	}
	got := codes(Judge("https://acme.example/", []Page{p}))
	for _, c := range got {
		code := strings.Split(c, "@")[0]
		if strings.HasPrefix(code, "title_") || strings.HasPrefix(code, "description_") || strings.HasPrefix(code, "canonical_") {
			t.Errorf("judged a dynamic value: %s (all: %v)", c, got)
		}
	}
	for _, want := range []string{"indexing_mismatch@/", "lang_missing@/", "og_tag_missing@/"} {
		if !slices.Contains(got, want) {
			t.Errorf("findings %v lack %s (robots index:false is a literal noindex; no lang; no openGraph)", got, want)
		}
	}
}

func TestNextLayoutWithoutMetadataHasNoTitle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, map[string]string{"app/layout.tsx": `export default function L({children}) { return <html lang="en">{children}</html>; }`})
	p, _ := NextLayout(dir, "web", "https://acme.example/")
	if got := codes(Judge("https://acme.example/", []Page{p})); !slices.Contains(got, "title_missing@/") {
		t.Errorf("findings %v lack title_missing", got)
	}

	write(t, dir, map[string]string{"app/layout.tsx": `export async function generateMetadata() { return { title: "x" }; }
export default function L({children}) { return <html lang="en">{children}</html>; }`})
	p, _ = NextLayout(dir, "web", "https://acme.example/")
	if got := codes(Judge("https://acme.example/", []Page{p})); slices.Contains(got, "title_missing@/") {
		t.Errorf("generateMetadata computes the head, yet the lane reported %v", got)
	}
}

func TestSiteOriginReadsTheHomePageFirst(t *testing.T) {
	t.Parallel()
	pages := []Page{
		{URL: "https://x.invalid/about", HTML: `<link rel="canonical" href="https://about.example/about">`},
		{URL: "https://x.invalid/", HTML: `<meta property="og:url" content="https://home.example/">`},
	}
	if got := SiteOrigin(pages); got != "https://home.example" {
		t.Errorf("SiteOrigin = %q, want the home page's og:url", got)
	}
	if got := SiteOrigin([]Page{{URL: "https://x.invalid/", HTML: "<title>t</title>"}}); got != "" {
		t.Errorf("SiteOrigin with nothing named = %q", got)
	}
}
