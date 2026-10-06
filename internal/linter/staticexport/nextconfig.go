package staticexport

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/linter/finding"
)

// nextConfigNames are the files Next.js loads its config from, in the order
// it looks.
var nextConfigNames = []string{"next.config.ts", "next.config.mts", "next.config.mjs", "next.config.js", "next.config.cjs"}

// loadNextConfig reads the frontend's next.config, nil when it has none.
func loadNextConfig(dir string) *sourceFile {
	for _, name := range nextConfigNames {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			return newSourceFile(name, string(raw))
		}
	}
	return nil
}

var (
	// outputExportRE matches an `output:` key whose value can be "export" —
	// a literal, or one arm of a conditional (`isProd ? "export" : …`).
	outputExportRE = regexp.MustCompile("\\boutput\\s*:\\s*[^,}\\n]*[\"'`]export[\"'`]")
	outputKeyRE    = regexp.MustCompile(`\boutput\s*:`)
	imagesKeyRE    = regexp.MustCompile(`\bimages\s*:`)
	// unoptimizedRE / customLoaderRE: the two ways next/image survives an
	// export — the optimizer off, or a loader that is not Next's server.
	unoptimizedRE  = regexp.MustCompile(`\bunoptimized\s*:\s*true\b`)
	customLoaderRE = regexp.MustCompile(`\bloader(File)?\s*:\s*["'` + "`" + `]`)
	// configRouteRE finds a rewrites / redirects / headers entry, written as
	// a method (`async rewrites() {`) or a property whose value is a function
	// (`rewrites: async () =>`) — Next.js accepts nothing else there. That is
	// also what keeps the `headers: [...]` INSIDE a header rule from
	// matching.
	configRouteRE = regexp.MustCompile(`\b(rewrites|redirects|headers)\s*(\(|:\s*(?:async\b|function\b|\(|[A-Za-z_$][\w$]*\s*=>))`)
	// devOnlyConditionRE is a condition that holds only in development.
	devOnlyConditionRE = regexp.MustCompile(`NODE_ENV\s*(===?\s*["']development["']|!==?\s*["']production["'])`)
	// prodEarlyReturnRE is a function body that returns before doing
	// anything in production: `if (process.env.NODE_ENV === "production") return …`.
	prodEarlyReturnRE = regexp.MustCompile(`^\s*\{?\s*if\s*\(\s*process\.env\.NODE_ENV\s*(===?\s*["']production["']|!==?\s*["']development["'])\s*\)\s*\{?\s*return\b`)
)

// configLine is the line to anchor a next.config finding on: the given key
// if present, else the line declaring the config object, else 1.
func configLine(cfg *sourceFile, key *regexp.Regexp) int {
	if loc := key.FindStringIndex(cfg.code); loc != nil {
		return cfg.lineAt(loc[0])
	}
	if i := strings.Index(cfg.code, "NextConfig"); i >= 0 {
		return cfg.lineAt(i)
	}
	return 1
}

// checkExported: the production build must BE an export before anything else
// about it matters. This is the lint twin of the render-time refusal
// (kcl/render.k _not_static_export), and it also catches the drift the
// render cannot see: forge.yaml and next.config disagreeing.
func (c *checker) checkExported() {
	declaredStatic := c.fe.DeclaredOutput == "static"
	cfg := c.nextConfig
	if cfg == nil {
		// No next.config at all is Next's default: a server build.
		c.add(RuleNotExported, finding.SeverityError, c.fe.RelDir, 0,
			fmt.Sprintf("%s, but it has no next.config, so its production build is a Next.js server, not a static export — there is nothing for a static runtime to publish", c.why()),
			"add a next.config.ts whose production build sets `output: \"export\"`"+c.exportTemplateHint())
		return
	}
	file := c.rel(cfg.rel)
	exported := outputExportRE.MatchString(cfg.code)
	switch {
	case exported && declaredStatic:
		return
	case exported:
		// The code exports; forge.yaml still says server. The render reads
		// forge.yaml, so it refuses the binding this build would satisfy.
		c.add(RuleNotExported, finding.SeverityError, file, configLine(cfg, outputExportRE),
			fmt.Sprintf("%s and %s sets `output: \"export\"`, but forge.yaml still declares `output: %s` for it — forge reads the build shape from forge.yaml, so the render refuses the binding", c.why(), cfg.rel, c.fe.DeclaredOutput),
			fmt.Sprintf("set `output: static` on frontend %q in forge.yaml so it says what the code does", c.fe.Name))
	case declaredStatic:
		c.add(RuleNotExported, finding.SeverityError, file, configLine(cfg, outputKeyRE),
			fmt.Sprintf("forge.yaml declares `output: static` for frontend %q, but %s never sets `output: \"export\"`: `next build` writes no out/, so a static runtime has nothing to publish", c.fe.Name, cfg.rel),
			"make the production build an export — `...(process.env.NODE_ENV === \"production\" ? { output: \"export\" } : {})` in the config object"+c.exportTemplateHint())
	default:
		c.add(RuleNotExported, finding.SeverityError, file, configLine(cfg, outputKeyRE),
			fmt.Sprintf("%s, but its build is not a static export: forge.yaml declares `output: %s` (a Node.js server) and %s does not set `output: \"export\"`, so there is nothing to publish — `forge env render` refuses this binding", c.why(), c.fe.DeclaredOutput, cfg.rel),
			fmt.Sprintf("either set `output: static` on frontend %q in forge.yaml, switch %s to the static export%s and fix the other static-export findings for this frontend; or bind it elsewhere — a Next.js server is a process, so it ships as a workload (fw.Workload with build = forge.DockerBuild {dockerfile = %q})",
				c.fe.Name, cfg.rel, c.exportTemplateHint(), path.Join(c.fe.RelDir, "Dockerfile")))
	}
}

// exportTemplateHint points at the scaffold's own static next.config, which
// `forge project upgrade` renders from forge.yaml's `output:`.
func (c *checker) exportTemplateHint() string {
	p := c.rel("next.config.ts")
	return fmt.Sprintf(" (with forge.yaml at `output: static`, `forge project upgrade --check %s` shows the scaffold's static next.config, and `forge project upgrade --force %s` adopts it)", p, p)
}

// checkNextImage: next/image's default loader is a server endpoint. The
// export refuses it unless the optimizer is off or a custom loader is set.
func (c *checker) checkNextImage() {
	cfg := c.nextConfig
	if cfg != nil && (unoptimizedRE.MatchString(cfg.code) || customLoaderRE.MatchString(cfg.code)) {
		return
	}
	var sites []string
	for _, f := range c.files {
		for _, imp := range importsOf(f, "next/image", "next/legacy/image") {
			sites = append(sites, fmt.Sprintf("%s:%d", c.rel(f.rel), imp.line))
		}
	}
	if len(sites) == 0 {
		return
	}
	file, line := c.fe.RelDir, 0
	if cfg != nil {
		file, line = c.rel(cfg.rel), configLine(cfg, imagesKeyRE)
	}
	shown := sites
	more := ""
	if len(shown) > 3 {
		more = fmt.Sprintf(" and %d more", len(shown)-3)
		shown = shown[:3]
	}
	c.add(RuleNextImage, finding.SeverityError, file, line,
		fmt.Sprintf("next/image is imported (%s%s), and its default loader is a server-side optimizer: the export fails with \"Image Optimization using the default loader is not compatible with export\"", strings.Join(shown, ", "), more),
		"set `images: { unoptimized: true }` in the next.config object (forge's static scaffold does), or a custom `images.loader` that resizes at a CDN"+authoritativeNote)
}

// checkConfigRoutes: rewrites, redirects and headers are applied by the
// Next.js server. A static host applies none of them, so each is a
// difference between `next dev` and production unless it is gated to dev.
func (c *checker) checkConfigRoutes() {
	cfg := c.nextConfig
	if cfg == nil {
		return
	}
	for _, loc := range configRouteRE.FindAllStringSubmatchIndex(cfg.code, -1) {
		key := cfg.code[loc[2]:loc[3]]
		if !isObjectKey(cfg.code, loc[0]) || devGated(cfg.code, loc[0], loc[1]) {
			continue
		}
		c.add(RuleConfigRoutes, finding.SeverityWarning, c.rel(cfg.rel), cfg.lineAt(loc[0]),
			fmt.Sprintf("next.config `%s` is not gated to development: a static host applies no %s, so every path it maps works under `next dev` and 404s (or loses its headers) in production — the export only warns", key, key),
			fmt.Sprintf("serve it from the backend or your CDN instead, or make the dev-only intent explicit: `...(process.env.NODE_ENV === \"development\" ? { async %s() { … } } : {})`", key))
	}
}

// isObjectKey reports whether the match at off begins an object member — the
// previous non-space character is `{`, `,` or the `async` keyword — rather
// than a call such as `headers()` from next/headers.
func isObjectKey(code string, off int) bool {
	before := strings.TrimRight(code[:off], " \t\r\n")
	if strings.HasSuffix(before, "async") {
		before = strings.TrimRight(strings.TrimSuffix(before, "async"), " \t\r\n")
	}
	return strings.HasSuffix(before, "{") || strings.HasSuffix(before, ",")
}

// devGated reports whether the member at [start,end) only exists, or only
// does anything, in development. Two shapes are recognised:
//
//   - its enclosing object literal is the true arm of a dev-only condition:
//     `...(process.env.NODE_ENV === "development" ? { rewrites() {…} } : {})`
//     or `...(isDev && { … })` where the condition names NODE_ENV;
//   - its body opens with a production early return:
//     `async rewrites() { if (process.env.NODE_ENV === "production") return []; …`.
func devGated(code string, start, end int) bool {
	if open := enclosingBrace(code, start); open > 0 {
		head := strings.TrimRight(code[:open], " \t\r\n")
		if strings.HasSuffix(head, "?") || strings.HasSuffix(head, "&&") {
			cond := head
			if i := strings.LastIndexAny(cond, ";{"); i >= 0 {
				cond = cond[i+1:]
			}
			if devOnlyConditionRE.MatchString(cond) {
				return true
			}
		}
	}
	// The member's body: from the first `{` after the key.
	rest := code[end:]
	if i := strings.Index(rest, "{"); i >= 0 && i < 200 {
		body := rest[i:]
		if len(body) > 300 {
			body = body[:300]
		}
		if prodEarlyReturnRE.MatchString(body) {
			return true
		}
	}
	return false
}

// enclosingBrace is the offset of the `{` that opens the object containing
// off, or -1. Strings are not tracked: config keys sit outside them.
func enclosingBrace(code string, off int) int {
	depth := 0
	for i := off - 1; i >= 0; i-- {
		switch code[i] {
		case '}':
			depth++
		case '{':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}
