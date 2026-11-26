// File: internal/cli/lint/lint_seo.go
//
// seo — `forge lint --seo` (Site insights, card F3).
//
// A static site's head decides how search engines index it and how link
// previews draw it, and nothing in a build fails when the title is missing,
// the canonical names another host or a stray noindex ships. This lane runs
// pkg/siteaudit's raw-HTML rules (the same rules `forge site audit` and the
// control plane's site insights run) over what the site would serve, offline
// (internal/linter/seo says how the pages are read).
//
// ── Which frontends it judges ─────────────────────────────────────────────
//
// Every Next.js or Vite frontend some env publishes as a static site — bound
// to forge.OnHosted, OnBucket or OnFirebase — read through each env's real
// render, like --static-export. The env that decides its public_dir,
// base_path and SITE_URL is prod when prod binds it, else the first env
// that does. Its built public_dir is judged when it holds an index.html,
// else the head its source declares (index.html for Vite, the root layout's
// metadata for Next.js).
//
// Every other frontend gets a "<name> (<type>): not applicable — <reason>"
// line, and a project with no frontend says so.
//
// The URL the pages are judged as is the env's SITE_URL build variable when
// it is a literal (Site insights design D3), else the origin the home page's
// canonical (or og:url) names, else http://localhost.
//
// ── Gating ────────────────────────────────────────────────────────────────
//
// Error findings gate (a missing title, a canonical to another host,
// noindex where the site should be indexed); warnings and info never do.
// The rules are new in this release, so they warn first: the rollout row in
// lint_rollout_table.go keeps their errors warnings until the next minor.

package lint

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/seo"
	"github.com/reliant-labs/forge/internal/linter/staticexport"
	"github.com/reliant-labs/forge/pkg/siteaudit"
)

// seoBinding is one env's static-site binding of a frontend: what the lane
// reads from the env's render.
type seoBinding struct {
	Env, PublicDir, BasePath, SiteURL string
}

// seoBindingsFunc reports, per frontend name, the envs that publish it as a
// static site. A seam so tests need not render KCL.
type seoBindingsFunc func(ctx context.Context, projectDir string, cfg *config.ProjectConfig) map[string][]seoBinding

// siteURLEnvVar is the build variable a site's canonical origin is baked
// from (forge.FrontendURL; Site insights design D3).
const siteURLEnvVar = "SITE_URL"

// renderSEOBindings renders every env under the deploy tree (through the
// run's KCL memo, so it shares --static-export's renders) and collects its
// static-site frontend bindings.
func renderSEOBindings(ctx context.Context, projectDir string, cfg *config.ProjectConfig) map[string][]seoBinding {
	kclDir := filepath.Join(projectDir, deployKCLDirFor(cfg))
	entries, err := os.ReadDir(kclDir)
	if err != nil {
		return nil
	}
	out := map[string][]seoBinding{}
	for _, e := range entries {
		envDir := filepath.Join(kclDir, e.Name())
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(envDir, "main.k")); err != nil {
			continue
		}
		raw, err := lintKCLRun(ctx, projectDir, envDir, []string{"env=" + strconv.Quote(e.Name())})
		if err != nil {
			continue
		}
		for _, f := range renderedSEOFrontends(raw) {
			if staticexport.IsStaticRuntime(f.runtime) {
				out[f.name] = append(out[f.name], seoBinding{Env: e.Name(), PublicDir: f.publicDir, BasePath: f.basePath, SiteURL: f.siteURL})
			}
		}
	}
	return out
}

type renderedSEOFrontend struct{ name, runtime, publicDir, basePath, siteURL string }

// renderedSEOFrontends reads output.frontends[] out of a render's JSON: the
// fields renderedFrontendRuntimes reads, plus the literal SITE_URL env var.
func renderedSEOFrontends(raw []byte) []renderedSEOFrontend {
	var doc struct {
		Output struct {
			Frontends []struct {
				Name    string `json:"name"`
				Runtime struct {
					Type string `json:"type"`
				} `json:"runtime"`
				PublicDir string `json:"public_dir"`
				BasePath  string `json:"base_path"`
				EnvVars   []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"env_vars"`
			} `json:"frontends"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	out := make([]renderedSEOFrontend, 0, len(doc.Output.Frontends))
	for _, f := range doc.Output.Frontends {
		r := renderedSEOFrontend{name: f.Name, runtime: f.Runtime.Type, publicDir: f.PublicDir, basePath: f.BasePath}
		for _, ev := range f.EnvVars {
			if ev.Name == siteURLEnvVar {
				r.siteURL = ev.Value
			}
		}
		out = append(out, r)
	}
	return out
}

// collectSEOJSON is the lane. Error findings gate.
func collectSEOJSON(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
	out, err := seoFindings(rc.ctx, rc.cwd, rc.cfg, renderSEOBindings)
	if err != nil {
		return nil, false, err
	}
	return out, anyErrorFinding(out), nil
}

// seoFindings judges every static-site frontend and returns its findings
// followed by one note per frontend.
func seoFindings(ctx context.Context, projectDir string, cfg *config.ProjectConfig, bindings seoBindingsFunc) ([]lintJSONFinding, error) {
	if cfg == nil || len(cfg.Frontends) == 0 {
		return []lintJSONFinding{ranNote("this project has no frontend — no site to judge for SEO")}, nil
	}
	var bound map[string][]seoBinding
	for _, fe := range cfg.Frontends {
		if _, ok := fe.Dir(projectDir); ok && (fe.IsNextJS() || isViteFrontend(fe)) {
			bound = bindings(ctx, projectDir, cfg)
			break
		}
	}
	var findings, notes []lintJSONFinding
	for _, fe := range cfg.Frontends {
		label := fmt.Sprintf("%s (%s)", fe.Name, frontendTypeLabel(fe))
		skip := func(reason string) {
			notes = append(notes, ranNote(fmt.Sprintf("%s: not applicable — %s", label, reason)))
		}
		rel, ok := fe.Dir(projectDir)
		switch {
		case !ok:
			skip("its code is not in this tree")
			continue
		case !fe.IsNextJS() && !isViteFrontend(fe):
			skip(fmt.Sprintf("the lane judges Next.js and Vite sites; a %s frontend is not one", frontendTypeLabel(fe)))
			continue
		case len(bound[fe.Name]) == 0:
			skip("no env publishes it as a static site (forge.OnHosted / OnBucket / OnFirebase)")
			continue
		}
		fs, note, err := seoJudgeFrontend(projectDir, rel, fe, pickSEOBinding(bound[fe.Name]))
		if err != nil {
			return nil, fmt.Errorf("seo lint: frontend %s: %w", fe.Name, err)
		}
		findings = append(findings, fs...)
		notes = append(notes, ranNote(label+": "+note))
	}
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].File < findings[j].File })
	return append(findings, notes...), nil
}

// pickSEOBinding is the binding that decides how the site is judged: prod's,
// else the first env's (envs are read in name order).
func pickSEOBinding(bs []seoBinding) seoBinding {
	if i := slices.IndexFunc(bs, func(b seoBinding) bool { return b.Env == "prod" }); i >= 0 {
		return bs[i]
	}
	return bs[0]
}

// seoPlaceholderSite is the base the pages are read under before the site's
// own origin is known.
const seoPlaceholderSite = "https://forge-seo-lint.invalid"

// seoJudgeFrontend judges one frontend's build, or its source when it is not
// built, and says which in its note.
func seoJudgeFrontend(projectDir, rel string, fe config.FrontendConfig, b seoBinding) ([]lintJSONFinding, string, error) {
	feDir := filepath.Join(projectDir, filepath.FromSlash(rel))
	publicDir := b.PublicDir
	if publicDir == "" {
		publicDir = "dist"
		if fe.IsNextJS() {
			publicDir = "out"
		}
	}
	base := "/" + strings.Trim(b.BasePath, "/")
	if base != "/" {
		base += "/"
	}
	tmpSite := seoPlaceholderSite + base

	var pages []seo.Page
	var note string
	builtDir := filepath.Join(feDir, filepath.FromSlash(publicDir))
	relBuilt := path.Join(rel, filepath.ToSlash(publicDir))
	if _, err := os.Stat(filepath.Join(builtDir, "index.html")); err == nil {
		ps, truncated, err := seo.BuiltPages(builtDir, relBuilt, tmpSite)
		if err != nil {
			return nil, "", err
		}
		pages = ps
		note = fmt.Sprintf("judged %d built page(s) in %s (env %s)", len(ps), relBuilt, b.Env)
		if truncated {
			note += fmt.Sprintf("; stopped at %d pages", seo.MaxPages)
		}
	} else {
		var p seo.Page
		var found bool
		if fe.IsNextJS() {
			p, found = seo.NextLayout(feDir, rel, tmpSite)
		} else {
			p, found = seo.ViteIndex(feDir, rel, tmpSite)
		}
		if !found {
			return nil, fmt.Sprintf("nothing to judge: %s is not built and has no index.html or root layout", relBuilt), nil
		}
		pages = []seo.Page{p}
		note = fmt.Sprintf("%s is not built, so only the head %s declares was judged; build it (npm run build) to judge every page, or run `forge site audit` for the full audit", relBuilt, p.File)
	}

	origin := ""
	if u, err := url.Parse(strings.TrimSpace(b.SiteURL)); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		origin = u.Scheme + "://" + u.Host
	}
	if origin == "" {
		origin = seo.SiteOrigin(pages)
	}
	if origin == "" {
		origin = "http://localhost"
	}
	site := origin + base
	for i := range pages {
		pages[i].URL = site + strings.TrimPrefix(pages[i].URL, tmpSite)
	}
	var out []lintJSONFinding
	for _, f := range seo.Judge(site, pages) {
		out = append(out, seoLintFinding(f))
	}
	return out, note, nil
}

// seoLintFinding is a siteaudit finding in lint's shape.
func seoLintFinding(f seo.Finding) lintJSONFinding {
	sev := lintSevInfo
	switch f.Severity {
	case siteaudit.SeverityError:
		sev = lintSevError
	case siteaudit.SeverityWarn:
		sev = lintSevWarning
	}
	var b strings.Builder
	route := f.Route
	if route == "" {
		route = "(site)"
	}
	fmt.Fprintf(&b, "%s: %s", route, strings.ReplaceAll(f.Code, "_", " "))
	if f.Ref != "" {
		fmt.Fprintf(&b, " (%s)", f.Ref)
	}
	if len(f.Evidence) > 0 {
		keys := make([]string, 0, len(f.Evidence))
		for k := range f.Evidence {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			v := f.Evidence[k]
			if len(v) > 100 {
				v = v[:97] + "..."
			}
			parts = append(parts, fmt.Sprintf("%s=%q", k, v))
		}
		b.WriteString(" — " + strings.Join(parts, " "))
	}
	return lintJSONFinding{File: f.File, Severity: sev, Rule: seo.RuleID(f.Code), Message: b.String(), FixHint: f.Fix}
}
