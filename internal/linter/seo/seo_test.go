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
	pages, _, truncated, err := BuiltPages(dir, "web/dist", "https://acme.example/")
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

func TestPageAtPlacesTheBaseRelativePathOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, map[string]string{"index.html": "<html></html>", "guide/index.html": "<html></html>", "guide/install.html": "<html></html>"})
	pages, _, _, err := BuiltPages(dir, "dist", PlaceholderOrigin+"/docs/")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range pages {
		got = append(got, p.At("https://acme.example/docs/").URL)
	}
	slices.Sort(got)
	want := []string{"https://acme.example/docs/", "https://acme.example/docs/guide/", "https://acme.example/docs/guide/install"}
	if !slices.Equal(got, want) {
		t.Errorf("URLs = %v, want %v", got, want)
	}
}

func TestSiteOriginNeverAnswersThePlaceholder(t *testing.T) {
	t.Parallel()
	pages := []Page{{URL: PlaceholderOrigin + "/", HTML: `<link rel="canonical" href="` + PlaceholderOrigin + `/">`}}
	if got := SiteOrigin(pages); got != "" {
		t.Errorf("SiteOrigin = %q, want \"\" for a canonical resolved against the placeholder", got)
	}
}

const bareRootLayout = `export const metadata = {
  openGraph: { title: "Acme", description: "Rockets.", type: "website", url: "https://acme.example/", images: ["https://acme.example/og.png"] },
  twitter: { card: "summary_large_image" },
};
export default function L({children}) { return <html lang="en"><body>{children}</body></html>; }
`

// B2: a page whose metadata comes from generateMetadata (params, data) is
// never reported missing what generateMetadata writes.
func TestNextPageGenerateMetadataIsNeverJudgedMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"app/layout.tsx": bareRootLayout,
		"app/page.tsx": `export async function generateMetadata() {
  const post = await load();
  return { title: post.title, description: post.summary, alternates: { canonical: "/" + post.slug } };
}
export default function Page() { return <main/>; }`,
	})
	p, ok := NextLayout(dir, "web", "https://acme.example/")
	if !ok {
		t.Fatal("no layout found")
	}
	for _, c := range codes(Judge("https://acme.example/", []Page{p})) {
		code := strings.Split(c, "@")[0]
		if strings.HasPrefix(code, "title_") || strings.HasPrefix(code, "description_") || strings.HasPrefix(code, "canonical_") {
			t.Errorf("reported %s although the page's generateMetadata writes it", c)
		}
	}

	// A literal set by a later segment is known again, and judged.
	write(t, dir, map[string]string{
		"app/layout.tsx": "export async function generateMetadata() { return {}; }\n" + strings.Replace(bareRootLayout, "export const metadata", "const unused", 1),
		"app/page.tsx":   `export const metadata = { title: "Hi" }; export default function P() { return null; }`,
	})
	p, _ = NextLayout(dir, "web", "https://acme.example/")
	got := codes(Judge("https://acme.example/", []Page{p}))
	if !slices.Contains(got, "title_length@/") {
		t.Errorf("findings %v lack title_length for the page's literal title \"Hi\"", got)
	}
	if slices.Contains(got, "description_missing@/") {
		t.Errorf("reported description_missing although the root layout's generateMetadata may write it: %v", got)
	}
}

// B3: the root route's head is composed from the root layout, the route
// groups' layouts and the root page; other segments do not count.
func TestNextRootRouteComposesNestedSegments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"src/app/layout.tsx": strings.Replace(bareRootLayout, "export const metadata = {",
			`export const metadata = {
  metadataBase: new URL("https://acme.example"),
  title: { default: "Acme", template: "%s | Acme Rockets" },`, 1),
		"src/app/(marketing)/layout.tsx": `export const metadata = { description: "Acme builds small, reliable rockets for hobbyists and schools, with kits that ship in a week." };
export default function M({children}) { return children; }`,
		"src/app/(marketing)/page.tsx": `export const metadata = { title: "Rockets for everyone", alternates: { canonical: "/" } };
export default function Home() { return <main/>; }`,
		"src/app/blog/layout.tsx": `export const metadata = { title: "Blog", robots: { index: false } };`,
	})
	p, ok := NextLayout(dir, "web", "https://acme.example/")
	if !ok {
		t.Fatal("no layout found")
	}
	h := siteaudit.ExtractHead(p.HTML)
	if h.Title != "Rockets for everyone | Acme Rockets" || h.Canonical != "https://acme.example/" || !strings.HasPrefix(h.Description, "Acme builds") || h.Robots != "" {
		t.Errorf("head = %+v\n%s", h, p.HTML)
	}
	if fs := Judge("https://acme.example/", []Page{p}); len(fs) != 0 {
		t.Errorf("a root route whose nested segments supply title, description and canonical: findings %v", codes(fs))
	}
}

// B1: object syntax the reader does not model makes the segment's whole
// metadata unknown, never a misread object whose missing fields are judged.
func TestNextMetadataUnmodelledSyntaxIsUnknown(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"computed key": `export const metadata = { [key]: "x", title: "Acme Rockets — rockets for everyone" };`,
		"method":       `export const metadata = { generate() { return 1 }, title: "Acme Rockets — rockets for everyone" };`,
		"getter":       `export const metadata = { get other() { return 1 }, title: "Acme Rockets — rockets for everyone" };`,
		"shorthand":    `export const metadata = { description, title: "Acme Rockets — rockets for everyone" };`,
		"run-on value": `export const metadata = { description: "a" + suffix, title: "Acme Rockets — rockets for everyone" };`,
	}
	for name, decl := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			write(t, dir, map[string]string{"app/layout.tsx": decl + "\nexport default function L({children}) { return <html lang=\"en\">{children}</html>; }"})
			p, ok := NextLayout(dir, "web", "https://acme.example/")
			if !ok {
				t.Fatal("no layout found")
			}
			for _, tag := range []string{"title", "description", "canonical", "og:title", "twitter:card"} {
				if !p.Dynamic[tag] {
					t.Errorf("%s is not dynamic: the metadata was read past syntax the reader does not model", tag)
				}
			}
			for _, c := range codes(Judge("https://acme.example/", []Page{p})) {
				if strings.HasPrefix(c, "title_") || strings.HasPrefix(c, "description_") || strings.HasPrefix(c, "canonical_") || strings.HasPrefix(c, "og_tag_missing") {
					t.Errorf("judged a field of unknown metadata: %s", c)
				}
			}
		})
	}
}

// B2: pages sharing a URL are all judged, each finding naming its own file,
// whatever order they arrive in.
func TestJudgeKeepsEveryPageThatSharesAURL(t *testing.T) {
	t.Parallel()
	good := `<html lang="en"><head><meta name="viewport" content="width=device-width"><title>Acme Rockets — rockets for everyone</title></head></html>`
	noTitle := `<html lang="en"><head><meta name="viewport" content="width=device-width"></head></html>`
	noLang := `<html><head><meta name="viewport" content="width=device-width"><title>Acme Rockets — rockets for everyone</title></head></html>`
	pages := []Page{
		{File: "dist/about.html", URL: "https://acme.example/about", HTML: noTitle},
		{File: "dist/About.html", URL: "https://ACME.example/about#x", HTML: noLang},
		{File: "dist/index.html", URL: "https://acme.example/", HTML: good},
	}
	byFile := func(fs []Finding) []string {
		var out []string
		for _, f := range fs {
			out = append(out, f.File+" "+f.Code)
		}
		slices.Sort(out)
		return out
	}
	got := byFile(Judge("https://acme.example/", pages))
	for _, want := range []string{"dist/about.html title_missing", "dist/About.html lang_missing"} {
		if !slices.Contains(got, want) {
			t.Errorf("findings %v lack %q", got, want)
		}
	}
	reversed := []Page{pages[2], pages[1], pages[0]}
	if again := byFile(Judge("https://acme.example/", reversed)); !slices.Equal(got, again) {
		t.Errorf("order changed the result:\n%v\n%v", got, again)
	}
}

// Two candidate files for one segment (layout.tsx beside layout.js) are
// Next.js's resolver's to settle, so the segment is unknown and named in a
// warning; the same for page files. A root app/ wins over src/app.
func TestNextAmbiguousSegmentFilesAreUnknown(t *testing.T) {
	t.Parallel()
	notitle := `export const metadata = { description: "x" };
export default function L({children}) { return <html lang="en">{children}</html>; }`
	titled := `export const metadata = { title: "Acme Rockets — rockets for everyone" };
export default function L({children}) { return <html lang="en">{children}</html>; }`
	cases := map[string]map[string]string{
		"root layout": {"app/layout.tsx": titled, "app/layout.js": notitle},
		"page":        {"app/layout.tsx": notitle, "app/page.tsx": `export const metadata = { title: "Acme Rockets — rockets for everyone" };`, "app/page.js": `export default function P() { return null; }`},
		"group layout": {"app/layout.tsx": notitle, "app/(site)/layout.tsx": titled, "app/(site)/layout.jsx": notitle,
			"app/(site)/page.tsx": `export default function P() { return null; }`},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			write(t, dir, files)
			p, ok := NextLayout(dir, "web", "https://acme.example/")
			if !ok {
				t.Fatal("no layout found")
			}
			if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "candidate") {
				t.Errorf("warnings = %q, want one naming both candidate files", p.Warnings)
			}
			if !p.Dynamic["title"] || !p.Dynamic["canonical"] {
				t.Errorf("dynamic = %v: the ambiguous segment's fields must be unknown", p.Dynamic)
			}
			for _, c := range codes(Judge("https://acme.example/", []Page{p})) {
				if strings.HasPrefix(c, "title_") || strings.HasPrefix(c, "canonical_") || strings.HasPrefix(c, "lang_missing") {
					t.Errorf("judged a field an ambiguous segment may write: %s", c)
				}
			}
		})
	}

	dir := t.TempDir()
	write(t, dir, map[string]string{"app/layout.tsx": titled, "src/app/layout.tsx": notitle})
	p, _ := NextLayout(dir, "web", "https://acme.example/")
	if p.File != "web/app/layout.tsx" || len(p.Warnings) != 0 || siteaudit.ExtractHead(p.HTML).Title == "" {
		t.Errorf("a root app/ beside src/app: file %q, warnings %q, head %s", p.File, p.Warnings, p.HTML)
	}
}

// An ambiguous segment leaves every metadata field it could write unknown,
// whatever a later segment sets: candidate layouts that disagree on
// robots.index or Open Graph never gate.
func TestNextAmbiguousRootLayoutLeavesEveryFieldUnknown(t *testing.T) {
	t.Parallel()
	noindex := `export const metadata = {
  title: "Acme Rockets — rockets for everyone",
  robots: { index: false },
  openGraph: { title: "Acme", description: "Rockets.", type: "website", url: "https://acme.example/", images: ["https://acme.example/og.png"] },
};
export default function L({children}) { return <html lang="en">{children}</html>; }`
	plain := `export default function L({children}) { return <html>{children}</html>; }`
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"app/layout.tsx": noindex,
		"app/layout.js":  plain,
		"app/page.tsx":   `export const metadata = { openGraph: { title: "Home" } }; export default function P() { return null; }`,
	})
	p, ok := NextLayout(dir, "web", "https://acme.example/")
	if !ok {
		t.Fatal("no layout found")
	}
	for _, tag := range []string{"title", "description", "canonical", "robots", "og:title", "og:description", "og:type", "og:url", "og:image", "twitter:card", "lang"} {
		if !p.Dynamic[tag] {
			t.Errorf("%s is judged although the root layout is ambiguous", tag)
		}
	}
	if fs := Judge("https://acme.example/", []Page{p}); len(fs) != 0 {
		t.Errorf("an ambiguous root layout produced findings: %v", codes(fs))
	}
}

// symlinkOrSkip makes link point at target, or skips where symlinks are not
// available.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
}

// The build is read through an os.Root: a symlinked file or directory that
// escapes it is skipped with a warning, an in-build symlink is followed.
func TestBuiltPagesNeverReadOutsideTheBuild(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	write(t, outside, map[string]string{"leak.html": "<title>OUTSIDE</title>", "dir/page.html": "<title>OUTSIDE</title>"})
	dir := t.TempDir()
	write(t, dir, map[string]string{"index.html": "<title>home</title>", "docs/guide.html": "<title>guide</title>"})
	symlinkOrSkip(t, filepath.Join(outside, "leak.html"), filepath.Join(dir, "leak.html"))
	symlinkOrSkip(t, filepath.Join(outside, "dir"), filepath.Join(dir, "escape"))
	symlinkOrSkip(t, filepath.Join("docs", "guide.html"), filepath.Join(dir, "alias.html"))
	symlinkOrSkip(t, "docs", filepath.Join(dir, "manual"))
	symlinkOrSkip(t, ".", filepath.Join(dir, "docs", "loop"))
	pages, warnings, _, err := BuiltPages(dir, "dist", "https://acme.example/")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, p := range pages {
		files = append(files, p.File)
		if strings.Contains(p.HTML, "OUTSIDE") {
			t.Errorf("%s: read a file outside the build", p.File)
		}
	}
	slices.Sort(files)
	for _, want := range []string{"dist/alias.html", "dist/manual/guide.html", "dist/index.html", "dist/docs/guide.html"} {
		if !slices.Contains(files, want) {
			t.Errorf("pages %v lack %s (an in-build symlink is followed)", files, want)
		}
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"skipped dist/leak.html", "skipped dist/escape"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q lack %q", warnings, want)
		}
	}
}

// The source fallback reads index.html and the Next.js segments through an
// os.Root too.
func TestSourceFallbackNeverReadsOutsideTheFrontend(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	write(t, outside, map[string]string{
		"index.html":   `<html><head><title>OUTSIDE</title></head></html>`,
		"layout.tsx":   `export const metadata = { title: "OUTSIDE title here" };`,
		"app/page.tsx": `export const metadata = { title: "OUTSIDE page" };`,
	})

	vite := t.TempDir()
	symlinkOrSkip(t, filepath.Join(outside, "index.html"), filepath.Join(vite, "index.html"))
	if p, ok := ViteIndex(vite, "web", "https://acme.example/"); ok || len(p.Warnings) != 1 {
		t.Errorf("an escaping index.html: ok %v, warnings %q; want skipped with a warning", ok, p.Warnings)
	}

	next := t.TempDir()
	write(t, next, map[string]string{"app/real.tsx": bareRootLayout})
	symlinkOrSkip(t, filepath.Join(outside, "layout.tsx"), filepath.Join(next, "app", "layout.js"))
	symlinkOrSkip(t, "real.tsx", filepath.Join(next, "app", "layout.tsx"))
	symlinkOrSkip(t, filepath.Join(outside, "app"), filepath.Join(next, "app", "(escape)"))
	p, ok := NextLayout(next, "web", "https://acme.example/")
	if !ok {
		t.Fatalf("the in-root symlinked layout.tsx was not read (warnings %q)", p.Warnings)
	}
	if strings.Contains(p.HTML, "OUTSIDE") || p.File != "web/app/layout.tsx" {
		t.Errorf("file %q head %s", p.File, p.HTML)
	}
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "skipped web/app/layout.js") {
		t.Errorf("warnings %q lack the escaping layout.js", p.Warnings)
	}
}
