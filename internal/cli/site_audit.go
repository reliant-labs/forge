package cli

// `forge site audit` — the local SEO and accessibility audit of a static
// site (Site insights, card F2).
//
// It runs the same rules the control plane runs after every promotion of a
// hosted site (pkg/siteaudit), over one of:
//
//   - a build directory, served in-process with hosted static hosting's
//     rules (internal/sitecrawl.DirHandler): no socket unless --browser
//     needs one for Chrome;
//   - the project's own site frontend, by default: its public_dir as the env
//     declares it (prod, else the first env), which must be built;
//   - a live URL, fetched through pkg/netguard: public addresses only, no
//     redirect followed implicitly.
//
// --browser adds the render pass: a Chrome already installed here renders
// each page (internal/sitecrawl/browser), the rules that read the DOM judge
// the rendered one, and axe-core scores accessibility when an npm install of
// axe-core is at hand. forge never downloads a browser.
//
// The --json document is pkg/siteaudit's Report — {rules_version, site,
// routes[], findings[]} — and a public surface: fields are only added.
// Exit codes: 0 no error finding, 1 error findings, 2 the audit could not
// run (nothing built, a refused URL, the site's root unreachable).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/sitecrawl"
	"github.com/reliant-labs/forge/internal/sitecrawl/browser"
	"github.com/reliant-labs/forge/pkg/siteaudit"
)

// siteAuditOptions are `forge site audit`'s flags.
type siteAuditOptions struct {
	jsonOut  bool
	browser  bool
	env      string
	frontend string
	siteURL  string
	maxPages int
}

func newSiteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "site",
		Short: "Static sites: audit a build or a live URL for SEO and accessibility",
		Long: `Static sites: the commands that judge a site as search engines, link
previews and assistive technology see it.

  forge site audit              audit the project's built site (or a directory, or a URL)`,
	}
	cmd.AddCommand(newSiteAuditCmd())
	return cmdutil.StrictGroup(cmd)
}

func newSiteAuditCmd() *cobra.Command {
	var o siteAuditOptions
	cmd := &cobra.Command{
		Use:   "audit [<dir>|<url>]",
		Short: "Audit a static site's SEO and accessibility: a build directory, the project's site, or a live URL",
		Long: `Audit a static site's SEO and accessibility with the rules Reliant's site
insights run after every promotion (pkg/siteaudit): titles, descriptions,
canonicals, social cards, indexing, robots.txt, sitemaps, broken links,
structured data, image alt text, and with --browser the rendered DOM and an
axe-core accessibility pass. Every finding names its fix. Scores are
Lighthouse-compatible (Lighthouse's SEO and accessibility audits and weights),
not Lighthouse itself.

What it audits:

  forge site audit              the project's site frontend, as built: its
                                public_dir as the env declares it (--env,
                                default prod); run 'npm run build' first
  forge site audit out/         a build directory, served in-process the way
                                Reliant hosting serves it
  forge site audit https://example.com
                                a live site, fetched over the network: public
                                addresses only, no redirect followed implicitly,
                                external links checked (capped)

A build's canonical and og:url name the host it was built for, so a directory
is audited as that host: --site-url, else the host its home page's canonical
(or og:url) names, else http://localhost.

--browser renders each page in a Chrome or Chromium already installed on this
machine ($CHROME_PATH, else PATH); forge never downloads one. Accessibility is
scored when axe-core is installed (npm i -D axe-core in the site), else the
report says it was not evaluated.

--json prints the report document, {rules_version, site, routes[], findings[]}
(stable: fields are only ever added).

Exit codes: 0 no error-severity finding; 1 one or more error findings; 2 the
audit could not run (nothing built, a refused URL, the site unreachable).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			return runSiteAudit(cmd.Context(), cmd.OutOrStdout(), target, o)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.jsonOut, "json", false, "print the report document as JSON")
	f.BoolVar(&o.browser, "browser", false, "render pages in a local Chrome (and run axe-core when installed)")
	f.StringVar(&o.env, "env", "", "env whose public_dir and base_path locate the project's build (default: prod, else the first env)")
	f.StringVar(&o.frontend, "frontend", "", "the site frontend to audit, when the project has several")
	f.StringVar(&o.siteURL, "site-url", "", "the URL a build directory is audited as (default: the host its canonical names)")
	f.IntVar(&o.maxPages, "max-pages", sitecrawl.DefaultMaxPages, "the most pages to crawl")
	return cmd
}

// siteAuditUnrunnable is exit 2: the audit could not run.
func siteAuditUnrunnable(format string, args ...any) error {
	return exitCodeError{code: exitUndetermined, msg: fmt.Sprintf(format, args...)}
}

// siteAuditTarget is what one audit crawls.
type siteAuditTarget struct {
	// dir and basePath are a build directory and its mount; dir is "" for a
	// URL audit.
	dir, basePath string
	// site is the URL the audit crawls as.
	site *url.URL
	// label names the target in the human report, and siteFrom says where
	// a build directory's site URL came from.
	label, siteFrom string
	// searchDirs are where axe-core is looked for.
	searchDirs []string
}

func runSiteAudit(ctx context.Context, out io.Writer, target string, o siteAuditOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	t, err := resolveSiteAuditTarget(ctx, target, o)
	if err != nil {
		return err
	}
	copts := sitecrawl.Options{Site: t.site.String(), MaxPages: o.maxPages}
	var notes []string
	if t.dir != "" {
		copts.Fetcher = sitecrawl.HandlerFetcher{Host: t.site.Host, Handler: sitecrawl.DirHandler(t.dir, t.basePath)}
		copts.Concurrency = 8
	} else {
		copts.Fetcher = sitecrawl.HTTPFetcher{Client: sitecrawl.GuardedClient(t.site, 15*time.Second)}
		copts.CheckExternal = true
		copts.RPS = 10
	}
	if o.browser {
		stop, err := attachRenderer(ctx, &copts, t, &notes)
		if err != nil {
			return err
		}
		defer stop()
	}
	res, err := sitecrawl.Crawl(ctx, copts)
	if err != nil {
		return siteAuditUnrunnable("%v", err)
	}
	if root := siteAuditRoot(res.Input); root != nil && root.Status == 0 {
		return siteAuditUnrunnable("could not fetch %s: %s", root.URL, root.Err)
	}
	notes = append(notes, res.Notes...)
	rep := siteaudit.Evaluate(res.Input)
	errs := 0
	for _, f := range rep.Findings {
		if f.Severity == siteaudit.SeverityError {
			errs++
		}
	}
	if o.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		writeSiteAuditReport(out, t.label, t.siteFrom, rep, notes)
	}
	if errs > 0 {
		return exitCodeError{code: exitWrong, msg: fmt.Sprintf("site audit: %d error finding(s)", errs), verdict: true}
	}
	return nil
}

// siteAuditRoot is the page fetched for the site's root.
func siteAuditRoot(in siteaudit.Input) *siteaudit.Page {
	for i := range in.Pages {
		if in.Pages[i].URL == in.Site {
			return &in.Pages[i]
		}
	}
	return nil
}

// attachRenderer starts Chrome and wires it into the crawl. A build
// directory is served on loopback for it, since Chrome cannot load an
// in-process handler.
func attachRenderer(ctx context.Context, copts *sitecrawl.Options, t siteAuditTarget, notes *[]string) (func(), error) {
	exe, err := browser.FindChrome()
	if err != nil {
		return nil, siteAuditUnrunnable("%v", err)
	}
	axe := ""
	if p := browser.FindAxeScript(t.searchDirs...); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, siteAuditUnrunnable("read %s: %v", p, err)
		}
		axe = string(b)
	} else {
		*notes = append(*notes, "accessibility not scored: axe-core is not installed (npm i -D axe-core in the site, then re-run with --browser)")
	}
	// Chrome reaches the network only through its proxy, which admits public
	// addresses (netguard) on the web's ports and the audited site's, and,
	// for a directory audit, the one loopback server that serves the build.
	popts := browser.ProxyOptions{Ports: []uint16{80, 443}}
	if p, err := strconv.ParseUint(t.site.Port(), 10, 16); err == nil && p != 80 && p != 443 {
		popts.Ports = append(popts.Ports, uint16(p))
	}
	var stops []func()
	stopAll := func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}
	if t.dir != "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, siteAuditUnrunnable("--browser: serve the build on loopback: %v", err)
		}
		srv := &http.Server{Handler: sitecrawl.DirHandler(t.dir, t.basePath), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		stops = append(stops, func() { _ = srv.Close() })
		local := ln.Addr().String()
		popts.AllowAddr = local
		copts.RenderURL = func(pageURL string) string {
			u, err := url.Parse(pageURL)
			if err != nil {
				return pageURL
			}
			u.Scheme, u.Host = "http", local
			return u.String()
		}
	}
	r, err := browser.Start(ctx, browser.Options{ExecPath: exe, AxeScript: axe, UserAgent: sitecrawl.UserAgent, Proxy: popts})
	if err != nil {
		stopAll()
		return nil, siteAuditUnrunnable("--browser: %v", err)
	}
	stops = append(stops, r.Close)
	copts.Renderer = r
	return stopAll, nil
}

// resolveSiteAuditTarget turns the argument into what to crawl.
func resolveSiteAuditTarget(ctx context.Context, target string, o siteAuditOptions) (siteAuditTarget, error) {
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		u, err := url.Parse(target)
		if err != nil || u.Host == "" {
			return siteAuditTarget{}, siteAuditUnrunnable("%q is not a URL", target)
		}
		if u.User != nil {
			return siteAuditTarget{}, siteAuditCredentials(u)
		}
		u.Fragment, u.RawFragment = "", ""
		if err := sitecrawl.RefusedHost(u); err != nil {
			return siteAuditTarget{}, siteAuditUnrunnable("%v. A local server's build is audited from its directory: %s site audit <dir>", err, Name())
		}
		cwd, _ := os.Getwd()
		return siteAuditTarget{site: siteBase(u), label: u.String(), searchDirs: []string{cwd}}, nil
	}
	if target != "" {
		fi, err := os.Stat(target)
		if err != nil || !fi.IsDir() {
			return siteAuditTarget{}, siteAuditUnrunnable("%s is not a directory or an http(s) URL", target)
		}
		site, from, err := siteAuditDirURL(target, "", o.siteURL, "")
		if err != nil {
			return siteAuditTarget{}, err
		}
		return siteAuditTarget{dir: target, site: site, label: target, siteFrom: from, searchDirs: []string{target}}, nil
	}
	return resolveProjectSite(ctx, o)
}

// siteAuditCredentials refuses a URL that carries user@ or user:pass@
// (exit 2). Credentials are never sent, and never trusted:
// https://trusted.example@127.0.0.1/ is a request to 127.0.0.1.
func siteAuditCredentials(u *url.URL) error {
	return siteAuditUnrunnable("the URL carries credentials (user@%s): an audit sends none, so give the site's URL without them", u.Hostname())
}

// siteBase is u as a site base: no query or fragment, path ending in "/".
func siteBase(u *url.URL) *url.URL {
	v := *u
	v.RawQuery, v.Fragment, v.RawFragment = "", "", ""
	if !strings.HasSuffix(v.Path, "/") {
		v.Path += "/"
		v.RawPath = ""
	}
	return &v
}

// siteAuditDirURL is the URL a build directory is audited as, and where it
// came from: --site-url; else the env's SITE_URL (envSiteURL, the value the
// build baked into its canonicals); else the origin the home page's
// canonical (or og:url) names; else http://localhost. Its path is basePath.
func siteAuditDirURL(dir, basePath, flag, envSiteURL string) (*url.URL, string, error) {
	base := "/" + strings.Trim(basePath, "/")
	if base != "/" {
		base += "/"
	}
	if flag != "" {
		u, err := url.Parse(flag)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, "", siteAuditUnrunnable("--site-url %q is not an absolute http(s) URL", flag)
		}
		if u.User != nil {
			return nil, "", siteAuditCredentials(u)
		}
		if u.Path == "" || u.Path == "/" {
			u.Path = base
		}
		return siteBase(u), "--site-url", nil
	}
	origin := func(raw string) *url.URL {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil
		}
		return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: base}
	}
	if u := origin(envSiteURL); u != nil {
		return u, "from the env's SITE_URL", nil
	}
	if b, err := sitecrawl.ReadFile(dir, "index.html"); err == nil {
		h := siteaudit.ExtractHead(string(b))
		if u := origin(h.Canonical); u != nil {
			return u, "from the home page's canonical", nil
		}
		if u := origin(h.Social["og:url"]); u != nil {
			return u, "from the home page's og:url", nil
		}
	}
	return &url.URL{Scheme: "http", Host: "localhost", Path: base}, "the home page names no canonical host", nil
}

// resolveProjectSite finds the project's built site frontend.
func resolveProjectSite(ctx context.Context, o siteAuditOptions) (siteAuditTarget, error) {
	cfg, err := loadProjectConfig()
	if err != nil {
		if errors.Is(err, ErrProjectConfigNotFound) {
			return siteAuditTarget{}, siteAuditUnrunnable("not in a forge project: pass a build directory or a URL (%s site audit <dir>|<url>)", Name())
		}
		return siteAuditTarget{}, siteAuditUnrunnable("%v", err)
	}
	projectDir := projectDirForKCL()
	var sites []config.FrontendConfig
	for _, fe := range cfg.Frontends {
		if _, ok := fe.Dir(projectDir); !ok {
			continue
		}
		if (fe.IsNextJS() && fe.StaticExport()) || isViteSite(fe) {
			sites = append(sites, fe)
		}
	}
	if o.frontend != "" {
		i := slices.IndexFunc(sites, func(fe config.FrontendConfig) bool { return fe.Name == o.frontend })
		if i < 0 {
			return siteAuditTarget{}, siteAuditUnrunnable("--frontend %s: no static site frontend of that name (have: %s)", o.frontend, siteFrontendNames(sites))
		}
		sites = sites[i : i+1]
	}
	switch len(sites) {
	case 0:
		return siteAuditTarget{}, siteAuditUnrunnable("this project has no static site frontend (a Vite app, or a Next.js static export): pass a build directory or a URL (%s site audit <dir>|<url>)", Name())
	case 1:
	default:
		return siteAuditTarget{}, siteAuditUnrunnable("this project has %d static site frontends (%s): pick one with --frontend", len(sites), siteFrontendNames(sites))
	}
	fe := sites[0]
	rel, _ := fe.Dir(projectDir)
	feDir := filepath.Join(projectDir, filepath.FromSlash(rel))
	publicDir, basePath, envSiteURL := siteBuildFacts(ctx, projectDir, fe, feDir, o.env)
	dir := filepath.Join(feDir, filepath.FromSlash(publicDir))
	if _, err := sitecrawl.ReadFile(dir, "index.html"); err != nil {
		return siteAuditTarget{}, siteAuditUnrunnable("%s is not built: %s has no index.html. Build it first (cd %s && npm run build), or pass a build directory", fe.Name, siteRelPath(dir), siteRelPath(feDir))
	}
	site, from, err := siteAuditDirURL(dir, basePath, o.siteURL, envSiteURL)
	if err != nil {
		return siteAuditTarget{}, err
	}
	return siteAuditTarget{dir: dir, basePath: basePath, site: site, label: siteRelPath(dir), siteFrom: from, searchDirs: []string{feDir, projectDir}}, nil
}

// siteBuildFacts is the frontend's public_dir, base_path and SITE_URL
// build variable as the env declares them, else the framework's convention
// (and no SITE_URL) when the env does not render.
func siteBuildFacts(ctx context.Context, projectDir string, fe config.FrontendConfig, feDir, env string) (publicDir, basePath, siteURL string) {
	if env == "" {
		envs := declaredEnvNames(projectDir)
		if slices.Contains(envs, "prod") {
			env = "prod"
		} else if len(envs) > 0 {
			env = envs[0]
		}
	}
	if env != "" {
		if ents, err := RenderKCL(ctx, projectDir, env); err == nil {
			for _, f := range ents.Frontends {
				if f.Name == fe.Name && f.PublicDir != "" {
					for _, ev := range f.EffectiveEnvVars() {
						if ev.Name == siteURLEnvVar {
							siteURL = ev.Value
						}
					}
					return f.PublicDir, f.BasePath, siteURL
				}
			}
		}
	}
	if fe.IsNextJS() {
		return "out", fe.BasePath, ""
	}
	if _, data, ok := readViteConfig(feDir); ok {
		if m := viteOutDirPattern.FindSubmatch(data); m != nil {
			return string(m[1]), "", ""
		}
	}
	return "dist", "", ""
}

// siteURLEnvVar is the build variable a site's canonical origin is baked
// from (Site insights design D3: env_vars.SITE_URL = forge.FrontendURL).
const siteURLEnvVar = "SITE_URL"

func isViteSite(fe config.FrontendConfig) bool {
	switch strings.ToLower(strings.TrimSpace(fe.Type)) {
	case "vite-spa", "vite":
		return true
	}
	return false
}

func siteFrontendNames(fes []config.FrontendConfig) string {
	names := make([]string, 0, len(fes))
	for _, fe := range fes {
		names = append(names, fe.Name)
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// siteRelPath is p relative to the working directory when it is under it.
func siteRelPath(p string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if rel, err := filepath.Rel(cwd, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return abs
}

// writeSiteAuditReport is the human report: the site's scores, a row per
// route, then every finding with its fix, errors first.
func writeSiteAuditReport(w io.Writer, label, siteFrom string, rep siteaudit.Report, notes []string) {
	switch siteFrom {
	case "":
		fmt.Fprintf(w, "Site audit of %s\n", rep.Site.URL)
	case "--site-url":
		fmt.Fprintf(w, "Site audit of %s, as %s\n", label, rep.Site.URL)
	default:
		fmt.Fprintf(w, "Site audit of %s, as %s (%s; --site-url audits it as another)\n", label, rep.Site.URL, siteFrom)
	}
	fmt.Fprintf(w, "Lighthouse-compatible scoring: SEO %s · Accessibility %s\n", scoreText(rep.Site.Score.SEO), scoreText(rep.Site.Score.A11y))
	if rep.Site.Score.A11y == nil {
		fmt.Fprintln(w, "  (accessibility is scored by axe-core in the --browser render pass)")
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROUTE\tSTATUS\tSEO\tA11Y")
	for _, r := range rep.Routes {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", r.Route, r.Status, scoreText(r.Score.SEO), scoreText(r.Score.A11y))
	}
	_ = tw.Flush()

	bySev := map[siteaudit.Severity][]siteaudit.Finding{}
	for _, f := range rep.Findings {
		bySev[f.Severity] = append(bySev[f.Severity], f)
	}
	fmt.Fprintf(w, "\nFindings: %d error, %d warn, %d info\n", len(bySev[siteaudit.SeverityError]), len(bySev[siteaudit.SeverityWarn]), len(bySev[siteaudit.SeverityInfo]))
	for _, sev := range []siteaudit.Severity{siteaudit.SeverityError, siteaudit.SeverityWarn, siteaudit.SeverityInfo} {
		for _, f := range bySev[sev] {
			where := f.Route
			if where == "" {
				where = "(site)"
			}
			line := fmt.Sprintf("  %-5s %s  %s", sev, f.Code, where)
			if f.Ref != "" {
				line += "  " + f.Ref
			}
			fmt.Fprintln(w, line)
			if ev := evidenceText(f.Evidence); ev != "" {
				fmt.Fprintf(w, "        %s\n", ev)
			}
			if f.Fix != "" {
				fmt.Fprintf(w, "        fix: %s\n", f.Fix)
			}
		}
	}
	for _, n := range notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
}

func scoreText(s *int) string {
	if s == nil {
		return "—"
	}
	return fmt.Sprintf("%d", *s)
}

// evidenceText is a finding's evidence as sorted key=value pairs.
func evidenceText(ev map[string]string) string {
	if len(ev) == 0 {
		return ""
	}
	keys := make([]string, 0, len(ev))
	for k := range ev {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := ev[k]
		if len(v) > 120 {
			v = v[:117] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%q", k, v))
	}
	return strings.Join(parts, " ")
}
