// Package seo is `forge lint --seo`'s analysis (Site insights, card F3): it
// runs pkg/siteaudit's raw-HTML rules ([siteaudit.EvaluateRaw]) over what a
// static site serves, offline, from the files in the tree:
//
//   - the built public_dir when it is there: every .html file, each at the
//     URL hosted static hosting serves it at (index.html is /, about.html is
//     /about, docs/index.html is /docs/), error pages (404.html,
//     _not-found.html) and Search Console verification files aside;
//   - else the source the export would emit the head of, best effort: a
//     Vite app's index.html, or a Next.js app's root layout metadata
//     (`export const metadata = {…}` in app/layout.tsx), rendered as the
//     <head> Next.js would write for it.
//
// What the raw HTML cannot show — statuses, headers, robots.txt, sitemaps,
// links that 404, the rendered DOM, axe — is `forge site audit`'s, and the
// control plane's after a promotion.
//
// A metadata value the parser cannot read as a literal (a variable, a
// template with ${}, a function call) is present but unknown: its tag is
// written with a stand-in value and every finding about that tag is dropped,
// so the lane never judges a value it did not see.
//
// Each finding's rule is "seo-" plus its siteaudit code in kebab case
// (seo-title-missing), so lint.rules dials each one.
package seo

import (
	"fmt"
	"html"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/pkg/siteaudit"
)

// RulePrefix starts every rule ID the lane emits.
const RulePrefix = "seo-"

// ruleIDs is each siteaudit code the lane reports, with its lint rule ID:
// the raw-HTML and DOM rules [siteaudit.EvaluateRaw] runs. It is spelled
// out rather than derived so a rule siteaudit adds joins `forge lint` only
// when someone adds it here and gives it a rollout row (lint_rollout_table.go):
// a finding whose code is missing is not reported, and
// TestRuleIDsCoverTheRawHTMLRules names the code to add.
var ruleIDs = map[string]string{
	"title_missing":           "seo-title-missing",
	"title_multiple":          "seo-title-multiple",
	"title_length":            "seo-title-length",
	"title_duplicate":         "seo-title-duplicate",
	"title_placeholder":       "seo-title-placeholder",
	"description_missing":     "seo-description-missing",
	"description_length":      "seo-description-length",
	"description_duplicate":   "seo-description-duplicate",
	"description_placeholder": "seo-description-placeholder",
	"canonical_missing":       "seo-canonical-missing",
	"canonical_conflicting":   "seo-canonical-conflicting",
	"canonical_relative":      "seo-canonical-relative",
	"canonical_invalid":       "seo-canonical-invalid",
	"canonical_foreign_host":  "seo-canonical-foreign-host",
	"canonical_to_root":       "seo-canonical-to-root",
	"og_tag_missing":          "seo-og-tag-missing",
	"twitter_card_missing":    "seo-twitter-card-missing",
	"og_image_relative":       "seo-og-image-relative",
	"indexing_mismatch":       "seo-indexing-mismatch",
	"hreflang_invalid":        "seo-hreflang-invalid",
	"hreflang_not_reciprocal": "seo-hreflang-not-reciprocal",
	"hreflang_no_x_default":   "seo-hreflang-no-x-default",
	"hreflang_missing_self":   "seo-hreflang-missing-self",
	"jsonld_invalid":          "seo-jsonld-invalid",
	"jsonld_no_context":       "seo-jsonld-no-context",
	"jsonld_missing_property": "seo-jsonld-missing-property",
	"link_not_crawlable":      "seo-link-not-crawlable",
	"link_text_generic":       "seo-link-text-generic",
	"link_text_empty":         "seo-link-text-empty",
	"image_alt_missing":       "seo-image-alt-missing",
	"lang_missing":            "seo-lang-missing",
	"viewport_missing":        "seo-viewport-missing",
}

// RuleID is the lint rule ID of a siteaudit finding code: "seo-" plus the
// code in kebab case.
func RuleID(code string) string {
	if id, ok := ruleIDs[code]; ok {
		return id
	}
	return RulePrefix + strings.ReplaceAll(code, "_", "-")
}

// RuleIDs are the rule IDs the lane can emit, in siteaudit's catalog order.
func RuleIDs() []string {
	var out []string
	for _, r := range siteaudit.Catalog() {
		if id, ok := ruleIDs[r.Code]; ok {
			out = append(out, id)
		}
	}
	return out
}

// MaxPages bounds the built pages judged in one frontend.
const MaxPages = 1000

// Page is one document judged: the file it came from (project-relative,
// slash-separated) and the URL it is served at.
type Page struct {
	File string
	URL  string
	HTML string
	// Dynamic names the head tags whose value the source does not state as
	// a literal ("title", "description", "canonical", "og:image", …).
	Dynamic map[string]bool
	// Rel is the page's path relative to the site's base ("" for its root,
	// "about", "docs/"): its URL is the site's base URL plus Rel, built once
	// the site's origin is known ([Page.At]).
	Rel string
}

// At is p served under site, the site's base URL ending in "/".
func (p Page) At(site string) Page {
	p.URL = site + p.Rel
	return p
}

// PlaceholderOrigin is the origin pages are read under before their site's
// own origin is known. [SiteOrigin] never answers it.
const PlaceholderOrigin = "https://forge-seo-lint.invalid"

// Finding is a siteaudit finding with the file it is about.
type Finding struct {
	siteaudit.Finding
	File string
}

// errorPages are the error pages a build writes beside its pages (hosted
// static hosting's 404.html; Next.js's _not-found.html and 500.html): never
// pages a search engine should index, so never judged as one.
var errorPages = map[string]bool{"404.html": true, "500.html": true, "_not-found.html": true}

// verificationFile matches a Search Console verification file.
var verificationFile = regexp.MustCompile(`^google[0-9a-f]+\.html$`)

// BuiltPages reads every page of a build directory, served under site (the
// site's base URL, ending in "/"). relDir is publicDir relative to the
// project, for the findings' files. truncated reports that MaxPages cut it.
func BuiltPages(publicDir, relDir, site string) (pages []Page, truncated bool, err error) {
	err = filepath.WalkDir(publicDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if p != publicDir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".html") || errorPages[name] || verificationFile.MatchString(name) {
			return nil
		}
		if len(pages) == MaxPages {
			truncated = true
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(publicDir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		route := routeOfFile(rel)
		pages = append(pages, Page{File: path.Join(relDir, rel), URL: site + route, Rel: route, HTML: string(b)})
		return nil
	})
	return pages, truncated, err
}

// routeOfFile is the path (relative to the site's base) hosted static
// hosting serves a build file at: index.html at its directory, x.html at x.
func routeOfFile(rel string) string {
	switch {
	case rel == "index.html":
		return ""
	case strings.HasSuffix(rel, "/index.html"):
		return strings.TrimSuffix(rel, "index.html")
	default:
		return strings.TrimSuffix(rel, ".html")
	}
}

// Judge runs the raw-HTML rules over pages as the site at site, and drops
// what a page's dynamic tags leave unknown.
func Judge(site string, pages []Page) []Finding {
	in := siteaudit.Input{Site: site}
	byRoute := map[string]Page{}
	for _, p := range pages {
		in.Pages = append(in.Pages, siteaudit.Page{URL: p.URL, RawHTML: p.HTML})
		if u, err := url.Parse(p.URL); err == nil {
			byRoute[routeOf(u)] = p
		}
	}
	rep := siteaudit.EvaluateRaw(in)
	var out []Finding
	for _, f := range rep.Findings {
		if _, reported := ruleIDs[f.Code]; !reported {
			continue
		}
		p, ok := byRoute[f.Route]
		if ok && p.Dynamic[dynamicTag(f)] {
			continue
		}
		out = append(out, Finding{Finding: f, File: p.File})
	}
	return out
}

// dynamicTag is the head tag a finding is about, when it is one a source can
// leave dynamic.
func dynamicTag(f siteaudit.Finding) string {
	switch {
	case strings.HasPrefix(f.Code, "title_"):
		return "title"
	case strings.HasPrefix(f.Code, "description_"):
		return "description"
	case strings.HasPrefix(f.Code, "canonical_"):
		return "canonical"
	case f.Code == "indexing_mismatch":
		return "robots"
	}
	return f.Ref
}

func routeOf(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// SiteOrigin is the origin the pages name for themselves: the home page's
// canonical, else its og:url, else the first canonical any page has. ""
// when none names one.
func SiteOrigin(pages []Page) string {
	origin := func(raw string) string {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			strings.HasSuffix(u.Host, dynamicHost) || u.Scheme+"://"+u.Host == PlaceholderOrigin {
			return ""
		}
		return u.Scheme + "://" + u.Host
	}
	sorted := append([]Page(nil), pages...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].URL) < len(sorted[j].URL) })
	if len(sorted) > 0 {
		h := siteaudit.ExtractHead(sorted[0].HTML)
		if o := origin(h.Canonical); o != "" {
			return o
		}
		if o := origin(h.Social["og:url"]); o != "" {
			return o
		}
	}
	for _, p := range sorted {
		if o := origin(siteaudit.ExtractHead(p.HTML).Canonical); o != "" {
			return o
		}
	}
	return ""
}

// ViteIndex is a Vite app's index.html, served at site.
func ViteIndex(feDir, relDir, site string) (Page, bool) {
	b, err := os.ReadFile(filepath.Join(feDir, "index.html"))
	if err != nil {
		return Page{}, false
	}
	return Page{File: path.Join(relDir, "index.html"), URL: site, HTML: string(b)}, true
}

// nextLayouts are where a Next.js app's root layout lives, in the order
// Next.js looks.
var nextLayouts = []string{
	"src/app/layout.tsx", "src/app/layout.jsx", "src/app/layout.ts", "src/app/layout.js",
	"app/layout.tsx", "app/layout.jsx", "app/layout.ts", "app/layout.js",
}

// NextLayout renders the head Next.js would emit for the app's root route,
// served at site: the metadata of the root layout, of every route-group
// layout on the way to the root page, and of that page, merged the way
// Next.js merges them (a later segment's top-level field replaces an
// earlier one's; a layout's title template wraps a later segment's title).
// ok is false when the app has no root layout.
//
// A segment whose metadata the reader cannot see — generateMetadata, a
// metadata that is not an object literal, a spread — may supply or replace
// any field, so every field it could touch is dynamic unless a later
// segment sets it literally: the lane never reports a tag missing that a
// segment might write.
func NextLayout(feDir, relDir, site string) (Page, bool) {
	for _, rel := range nextLayouts {
		b, err := os.ReadFile(filepath.Join(feDir, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		appDir := filepath.Join(feDir, filepath.FromSlash(path.Dir(rel)))
		srcs := append([]string{string(b)}, rootRouteSegments(appDir)...)
		doc, dynamic := nextHead(srcs, site)
		return Page{File: path.Join(relDir, rel), URL: site, HTML: doc, Dynamic: dynamic}, true
	}
	return Page{}, false
}

// nextSourceExts are the extensions a Next.js segment file may have.
var nextSourceExts = []string{".tsx", ".jsx", ".ts", ".js"}

// readSegmentFile returns dir/<name>.<ext> for the first extension present.
func readSegmentFile(dir, name string) (string, bool) {
	for _, ext := range nextSourceExts {
		if b, err := os.ReadFile(filepath.Join(dir, name+ext)); err == nil {
			return string(b), true
		}
	}
	return "", false
}

// rootRouteSegments are the sources, after the root layout, that make the
// root route's head: the layouts of the route groups ("(marketing)") the
// root page sits in, outermost first, then the page. Empty when the root
// page is not found.
func rootRouteSegments(appDir string) []string {
	var walk func(dir string, depth int) ([]string, bool)
	walk = func(dir string, depth int) ([]string, bool) {
		var chain []string
		if depth > 0 {
			if l, ok := readSegmentFile(dir, "layout"); ok {
				chain = append(chain, l)
			}
		}
		if pg, ok := readSegmentFile(dir, "page"); ok {
			return append(chain, pg), true
		}
		if depth == 8 {
			return nil, false
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, false
		}
		for _, e := range entries {
			if n := e.Name(); e.IsDir() && strings.HasPrefix(n, "(") && strings.HasSuffix(n, ")") {
				if rest, ok := walk(filepath.Join(dir, n), depth+1); ok {
					return append(chain, rest...), true
				}
			}
		}
		return nil, false
	}
	chain, _ := walk(appDir, 0)
	return chain
}

// dynamicHost is the host of the stand-in URL a dynamic URL value gets.
const dynamicHost = "forge-dynamic.invalid"

var (
	metadataDecl   = regexp.MustCompile(`export\s+const\s+metadata\s*(?::\s*[A-Za-z_.<>]+\s*)?=\s*\{`)
	metadataExport = regexp.MustCompile(`export\s+(?:const|let|var)\s+metadata\b|export\s*\{[^}]*\bmetadata\b[^}]*\}`)
	htmlLang       = regexp.MustCompile(`<html[^>]*\blang\s*=\s*(?:"([^"]*)"|'([^']*)'|\{\s*["']([^"']*)["']\s*\})`)
	newURLCall     = regexp.MustCompile(`^new\s+URL\(\s*(?:"([^"]*)"|'([^']*)'|` + "`([^`$]*)`" + `)\s*\)$`)
)

// spreadKey marks an object literal that spreads another object into it.
const spreadKey = "\x00spread"

// metadataTags are the head tags each top-level metadata field writes.
var metadataTags = map[string][]string{
	"title":       {"title"},
	"description": {"description"},
	"alternates":  {"canonical"},
	"robots":      {"robots"},
	"openGraph":   {"og:title", "og:description", "og:type", "og:url", "og:image"},
	"twitter":     {"twitter:card"},
}

// segmentMetadata reads one segment file's static metadata. known is false
// when the segment computes or hides it (generateMetadata, a metadata that
// is not an object literal, a spread): it may then write any field.
func segmentMetadata(src string) (meta map[string]jsValue, known bool) {
	if strings.Contains(src, "generateMetadata") {
		return nil, false
	}
	if loc := metadataDecl.FindStringIndex(src); loc != nil {
		p := &jsParser{s: src, i: loc[1] - 1}
		v, ok := p.value().(map[string]jsValue)
		if !ok {
			return nil, false
		}
		if _, spread := v[spreadKey]; spread {
			return v, false
		}
		return v, true
	}
	if metadataExport.MatchString(src) {
		return nil, false
	}
	return nil, true
}

// mergeSegments merges the segments' metadata the way Next.js does, and
// names the tags a segment the reader cannot see may write.
func mergeSegments(srcs []string) (map[string]jsValue, map[string]bool) {
	dynamic := map[string]bool{}
	meta := map[string]jsValue{}
	unknownSince := false           // a segment the reader cannot see came before
	knownAfter := map[string]bool{} // fields set literally after it
	template := ""
	for i, src := range srcs {
		m, known := segmentMetadata(src)
		if !known {
			unknownSince = true
			knownAfter = map[string]bool{}
		}
		for k, v := range m {
			if k == spreadKey {
				continue
			}
			if k == "title" && i > 0 && template != "" {
				if t, lit := jsString(v); lit {
					v = strings.ReplaceAll(template, "%s", t)
				}
			}
			meta[k] = v
			if known {
				knownAfter[k] = true
			}
		}
		// A layout's title template applies to the segments below it.
		if obj, ok := m["title"].(map[string]jsValue); ok && i < len(srcs)-1 {
			if t, lit := jsString(obj["template"]); lit {
				template = t
			}
		}
	}
	if unknownSince {
		for field, tags := range metadataTags {
			if !knownAfter[field] {
				for _, t := range tags {
					dynamic[t] = true
				}
			}
		}
	}
	return meta, dynamic
}

// nextHead is the document the segments' merged metadata gives the root
// route (srcs[0] is the root layout): the tags Next.js writes for the fields
// it can read, and which of them are dynamic.
func nextHead(srcs []string, site string) (string, map[string]bool) {
	meta, dynamic := mergeSegments(srcs)
	src := srcs[0]
	base, _ := url.Parse(site)
	if mb, ok := meta["metadataBase"]; ok {
		if s, lit := jsString(mb); lit {
			if u, err := url.Parse(s); err == nil && u.IsAbs() {
				base = u
			}
		}
	}
	var head strings.Builder
	head.WriteString(`<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`)
	tag := func(name, value string) {
		if value == "" {
			return
		}
		v := html.EscapeString(value)
		switch {
		case name == "title":
			head.WriteString("<title>" + v + "</title>")
		case name == "canonical":
			head.WriteString(`<link rel="canonical" href="` + v + `">`)
		case strings.HasPrefix(name, "og:"):
			head.WriteString(`<meta property="` + name + `" content="` + v + `">`)
		default:
			head.WriteString(`<meta name="` + name + `" content="` + v + `">`)
		}
	}
	// field writes one tag from a metadata value: a literal as itself, a
	// value it cannot read as a stand-in, marked dynamic.
	field := func(name string, v jsValue, isURL bool) {
		if v == nil {
			return
		}
		s, lit := jsString(v)
		if !lit {
			dynamic[name] = true
			s = "dynamic value"
			if isURL {
				s = "https://" + dynamicHost + "/"
			}
		} else if isURL && base != nil {
			if u, err := base.Parse(s); err == nil {
				s = u.String()
			}
		}
		tag(name, s)
	}
	title := meta["title"]
	if obj, ok := title.(map[string]jsValue); ok {
		title = obj["absolute"]
		if title == nil {
			title = obj["default"]
		}
	}
	field("title", title, false)
	field("description", meta["description"], false)
	if alt, ok := meta["alternates"].(map[string]jsValue); ok {
		field("canonical", alt["canonical"], true)
	} else if meta["alternates"] != nil {
		dynamic["canonical"] = true
	}
	switch r := meta["robots"].(type) {
	case nil:
	case map[string]jsValue:
		if s, lit := jsString(r["index"]); lit && s == "false" {
			tag("robots", "noindex")
		} else if r["index"] != nil && !lit {
			dynamic["robots"] = true
		}
	default:
		field("robots", r, false)
	}
	if og, ok := meta["openGraph"].(map[string]jsValue); ok {
		field("og:title", og["title"], false)
		field("og:description", og["description"], false)
		field("og:url", og["url"], true)
		field("og:type", og["type"], false)
		img := og["images"]
		if arr, ok := img.([]jsValue); ok {
			img = nil
			if len(arr) > 0 {
				img = arr[0]
			}
		}
		if obj, ok := img.(map[string]jsValue); ok {
			img = obj["url"]
		}
		field("og:image", img, true)
	} else if meta["openGraph"] != nil {
		for _, t := range []string{"og:title", "og:description", "og:url", "og:type", "og:image"} {
			dynamic[t] = true
		}
	}
	if tw, ok := meta["twitter"].(map[string]jsValue); ok {
		field("twitter:card", tw["card"], false)
	} else if meta["twitter"] != nil {
		dynamic["twitter:card"] = true
	}
	langAttr := ""
	if m := htmlLang.FindStringSubmatch(src); len(m) == 4 && m[1]+m[2]+m[3] != "" {
		langAttr = ` lang="` + html.EscapeString(m[1]+m[2]+m[3]) + `"`
	}
	return fmt.Sprintf("<!doctype html><html%s><head>%s</head><body></body></html>", langAttr, head.String()), dynamic
}

// jsValue is a parsed JavaScript value: a string (a literal, or a keyword or
// number as written), map[string]jsValue, []jsValue, or jsDynamic.
type jsValue any

// jsDynamic is a value the parser does not evaluate; its text is kept.
type jsDynamic string

// jsString reads v as a literal string; false for anything else. A
// `new URL("…")` call counts as its argument.
func jsString(v jsValue) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case jsDynamic:
		if m := newURLCall.FindStringSubmatch(strings.TrimSpace(string(x))); len(m) == 4 {
			return m[1] + m[2] + m[3], true
		}
	}
	return "", false
}

// jsParser reads a JavaScript object literal: enough of the syntax for
// Next.js metadata, never evaluating anything.
type jsParser struct {
	s string
	i int
}

func (p *jsParser) skipSpace() {
	for p.i < len(p.s) {
		switch {
		case strings.HasPrefix(p.s[p.i:], "//"):
			if j := strings.IndexByte(p.s[p.i:], '\n'); j >= 0 {
				p.i += j + 1
			} else {
				p.i = len(p.s)
			}
		case strings.HasPrefix(p.s[p.i:], "/*"):
			if j := strings.Index(p.s[p.i+2:], "*/"); j >= 0 {
				p.i += j + 4
			} else {
				p.i = len(p.s)
			}
		case strings.ContainsRune(" \t\r\n", rune(p.s[p.i])):
			p.i++
		default:
			return
		}
	}
}

func (p *jsParser) value() jsValue {
	p.skipSpace()
	if p.i >= len(p.s) {
		return jsDynamic("")
	}
	switch c := p.s[p.i]; c {
	case '{':
		return p.object()
	case '[':
		return p.array()
	case '"', '\'', '`':
		if s, ok := p.str(); ok {
			return s
		}
		return jsDynamic("template")
	default:
		start := p.i
		p.skipExpr()
		text := strings.TrimSpace(p.s[start:p.i])
		switch text {
		case "true", "false", "null":
			return text
		}
		return jsDynamic(text)
	}
}

// str reads a string literal; false for a template with ${…}.
func (p *jsParser) str() (string, bool) {
	q := p.s[p.i]
	p.i++
	var b strings.Builder
	literal := true
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '\\' && p.i+1 < len(p.s):
			b.WriteByte(p.s[p.i+1])
			p.i += 2
			continue
		case c == q:
			p.i++
			return b.String(), literal
		case q == '`' && c == '$' && p.i+1 < len(p.s) && p.s[p.i+1] == '{':
			literal = false
		}
		b.WriteByte(c)
		p.i++
	}
	return b.String(), false
}

func (p *jsParser) object() map[string]jsValue {
	out := map[string]jsValue{}
	p.i++ // {
	for {
		p.skipSpace()
		if p.i >= len(p.s) {
			return out
		}
		if p.s[p.i] == '}' {
			p.i++
			return out
		}
		if strings.HasPrefix(p.s[p.i:], "...") {
			p.i += 3
			p.skipExpr()
			p.comma()
			out[spreadKey] = jsDynamic("...")
			continue
		}
		var key string
		if c := p.s[p.i]; c == '"' || c == '\'' {
			key, _ = p.str()
		} else {
			start := p.i
			for p.i < len(p.s) && (isIdent(p.s[p.i])) {
				p.i++
			}
			key = p.s[start:p.i]
			if key == "" { // something we cannot read: skip it
				p.skipExpr()
				p.comma()
				continue
			}
		}
		p.skipSpace()
		if p.i < len(p.s) && p.s[p.i] == ':' {
			p.i++
			out[key] = p.value()
		} else {
			out[key] = jsDynamic(key) // shorthand {title}
		}
		p.comma()
	}
}

func (p *jsParser) array() []jsValue {
	var out []jsValue
	p.i++ // [
	for {
		p.skipSpace()
		if p.i >= len(p.s) {
			return out
		}
		if p.s[p.i] == ']' {
			p.i++
			return out
		}
		out = append(out, p.value())
		p.comma()
	}
}

func (p *jsParser) comma() {
	p.skipSpace()
	if p.i < len(p.s) && p.s[p.i] == ',' {
		p.i++
	}
}

// skipExpr moves past an expression to the next ',' or closing bracket at
// its own depth.
func (p *jsParser) skipExpr() {
	depth := 0
	for p.i < len(p.s) {
		switch c := p.s[p.i]; c {
		case '"', '\'', '`':
			p.str()
			continue
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return
			}
			depth--
		case ',':
			if depth == 0 {
				return
			}
		}
		p.i++
	}
}

func isIdent(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
