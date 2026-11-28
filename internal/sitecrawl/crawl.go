package sitecrawl

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"

	"golang.org/x/time/rate"

	"github.com/reliant-labs/forge/pkg/siteaudit"
)

// Default caps. A local build is cheap to crawl; the caps keep a large site,
// or a URL audit of someone's production host, bounded.
const (
	DefaultMaxPages      = 100
	DefaultMaxLinkChecks = 300
	DefaultMaxExternal   = 50
	DefaultMaxRender     = 25
	DefaultConcurrency   = 4

	maxPageBytes    = 5 << 20
	maxImageBytes   = 4 << 20
	maxLinkGetBytes = 64 << 10
	maxSitemapFiles = 20
	maxRedirectHops = 5
)

// NotFoundProbePath is the path, under the site's base, fetched to learn how
// the site answers a page that does not exist (soft-404 detection).
const NotFoundProbePath = "forge-site-audit-not-a-page"

// Renderer renders a page in a headless browser and returns its DOM
// serialized after JavaScript ran, and axe-core's result when it ran axe
// (nil when it did not).
type Renderer interface {
	Render(ctx context.Context, pageURL string) (html string, axe *siteaudit.AxeResult, err error)
}

// Options configure a crawl. Site and Fetcher are required.
type Options struct {
	// Site is the site's base URL: scheme, host and base path.
	Site string
	// Fetcher fetches every URL the crawl reads.
	Fetcher Fetcher
	// MaxPages caps the pages fetched and judged (default DefaultMaxPages).
	MaxPages int
	// MaxLinkChecks caps the status checks of internal targets that are not
	// crawled as pages, og:images included (default DefaultMaxLinkChecks).
	MaxLinkChecks int
	// CheckExternal checks the status of links to other hosts, up to
	// MaxExternal (default DefaultMaxExternal).
	CheckExternal bool
	MaxExternal   int
	// Concurrency bounds the fetches in flight (default DefaultConcurrency).
	Concurrency int
	// RPS bounds the fetches started per second; 0 is unbounded.
	RPS float64
	// Renderer, when set, renders up to MaxRender pages (default
	// DefaultMaxRender), root first.
	Renderer  Renderer
	MaxRender int
	// RenderURL maps a page's URL to the URL the renderer loads (a local
	// build served on loopback); nil loads the page's own URL.
	RenderURL func(pageURL string) string
}

// Result is a crawl's facts and what the crawl could not do.
type Result struct {
	// Input is what siteaudit judges.
	Input siteaudit.Input
	// Notes say what the crawl left out (caps reached, links not checked,
	// pages that failed to render), one line each.
	Notes []string
}

// Crawl fetches the site and returns its facts. It fails only when Site is
// not an absolute http(s) URL; every fetch failure is a fact.
func Crawl(ctx context.Context, o Options) (Result, error) {
	site, err := url.Parse(o.Site)
	if err != nil || (site.Scheme != "http" && site.Scheme != "https") || site.Host == "" {
		return Result{}, fmt.Errorf("site %q is not an absolute http(s) URL", o.Site)
	}
	if site.User != nil {
		return Result{}, errors.New("the site URL carries credentials (user@host): an audit sends none")
	}
	site.Fragment, site.RawFragment, site.RawQuery = "", "", ""
	if !strings.HasSuffix(site.Path, "/") {
		site.Path += "/"
	}
	site.RawPath = ""
	if o.MaxPages <= 0 {
		o.MaxPages = DefaultMaxPages
	}
	if o.MaxLinkChecks <= 0 {
		o.MaxLinkChecks = DefaultMaxLinkChecks
	}
	if o.MaxExternal <= 0 {
		o.MaxExternal = DefaultMaxExternal
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.MaxRender <= 0 {
		o.MaxRender = DefaultMaxRender
	}
	c := &crawler{o: o, site: site, seen: map[string]bool{}, checked: map[string]bool{}}
	if o.RPS > 0 {
		c.limit = rate.NewLimiter(rate.Limit(o.RPS), 1)
	}
	c.in.Site = site.String()
	c.robots(ctx)
	c.sitemaps(ctx)
	c.pages(ctx)
	c.linkChecks(ctx)
	c.notFound(ctx)
	c.render(ctx)
	return Result{Input: c.in, Notes: c.notes}, nil
}

type crawler struct {
	o       Options
	site    *url.URL
	limit   *rate.Limiter
	in      siteaudit.Input
	notes   []string
	robot   *siteaudit.Robots
	seeds   []string
	seen    map[string]bool // page URLs queued or fetched (normalized)
	queue   []string
	rest    []string        // same-site page URLs past the page cap
	assets  []string        // same-site non-page targets
	images  []string        // og:image targets
	extern  []string        // other hosts' targets
	checked map[string]bool // targets fetched as pages or queued for a check
}

// fetch is the one way the crawl requests anything. A URL a page named with
// credentials in it (https://user:pass@host/) is requested without them:
// the audit never sends what a page planted.
func (c *crawler) fetch(ctx context.Context, method, u string, max int64) Response {
	u = WithoutUserinfo(u)
	if c.limit != nil {
		if err := c.limit.Wait(ctx); err != nil {
			return Response{Err: err.Error()}
		}
	}
	return c.o.Fetcher.Fetch(ctx, method, u, max)
}

// origin is the site's scheme and host.
func (c *crawler) origin() string { return c.site.Scheme + "://" + c.site.Host }

// sameSite reports a URL on the site's host, under its base path.
func (c *crawler) sameSite(u *url.URL) bool {
	if !strings.EqualFold(u.Host, c.site.Host) {
		return false
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	return strings.HasPrefix(p, c.site.Path) || p+"/" == c.site.Path
}

// pageLike reports a path that names a page: a directory, no extension, or
// .html.
func pageLike(u *url.URL) bool {
	p := u.Path
	if p == "" || strings.HasSuffix(p, "/") {
		return true
	}
	switch strings.ToLower(path.Ext(p)) {
	case ".html", ".htm":
		return true
	}
	return routeShaped(p)
}

// allowed reports whether robots.txt lets the crawler fetch u. The root is
// always fetched.
func (c *crawler) allowed(u *url.URL) bool {
	if c.robot == nil || u.Path == c.site.Path {
		return true
	}
	return c.robot.Allowed(UserAgentToken, routeOf(u))
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

// norm is a URL without its fragment, host lower-cased, an empty path "/".
func norm(u *url.URL) string {
	v := *u
	v.User = nil
	v.Fragment, v.RawFragment = "", ""
	v.Host = strings.ToLower(v.Host)
	v.Scheme = strings.ToLower(v.Scheme)
	if v.Path == "" {
		v.Path, v.RawPath = "/", ""
	}
	return v.String()
}

func (c *crawler) robots(ctx context.Context) {
	u := c.origin() + "/robots.txt"
	r := c.fetch(ctx, http.MethodGet, u, 600<<10)
	c.in.Robots = &siteaudit.Fetched{URL: u, Status: r.Status, Body: string(r.Body), Err: r.Err}
	if r.Status >= 200 && r.Status < 300 {
		c.robot = siteaudit.ParseRobots(string(r.Body))
	}
}

func (c *crawler) sitemaps(ctx context.Context) {
	var todo []string
	listed := map[string]bool{}
	if c.robot != nil {
		for _, s := range c.robot.Sitemaps {
			u, err := url.Parse(strings.TrimSpace(s))
			if err != nil || !strings.EqualFold(u.Host, c.site.Host) {
				continue
			}
			if !listed[norm(u)] {
				listed[norm(u)] = true
				todo = append(todo, u.String())
			}
		}
	}
	fallback := ""
	if len(todo) == 0 {
		fallback = c.origin() + c.site.Path + "sitemap.xml"
		todo = append(todo, fallback)
	}
	done := map[string]bool{}
	for len(todo) > 0 && len(done) < maxSitemapFiles {
		u := todo[0]
		todo = todo[1:]
		if done[u] {
			continue
		}
		done[u] = true
		r := c.fetch(ctx, http.MethodGet, u, siteaudit.SitemapMaxBytes)
		if u == fallback && (r.Status < 200 || r.Status >= 300) {
			continue // not declared, not there: nothing to judge
		}
		body := r.Body
		if r.Status >= 200 && r.Status < 300 && isGzip(u, body) {
			if b, err := gunzip(body); err == nil {
				body = b
			} else {
				r.Err, r.Status = "gzip: "+err.Error(), 0
			}
		}
		c.in.Sitemaps = append(c.in.Sitemaps, siteaudit.Fetched{URL: u, Status: r.Status, Body: string(body), Err: r.Err})
		if r.Status < 200 || r.Status >= 300 {
			continue
		}
		sm, err := siteaudit.ParseSitemap(string(body))
		if err != nil {
			continue
		}
		for _, loc := range sm.Locs {
			lu, err := url.Parse(loc)
			if err != nil || !lu.IsAbs() {
				continue
			}
			if sm.Index {
				if strings.EqualFold(lu.Host, c.site.Host) {
					todo = append(todo, lu.String())
				}
				continue
			}
			if c.sameSite(lu) {
				c.seeds = append(c.seeds, lu.String())
			}
		}
	}
	if len(todo) > 0 {
		c.notes = append(c.notes, fmt.Sprintf("read %d sitemap files; %d more were not read", len(done), len(todo)))
	}
}

func isGzip(u string, b []byte) bool {
	return strings.HasSuffix(strings.ToLower(u), ".gz") || (len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b)
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(io.LimitReader(zr, siteaudit.SitemapMaxBytes+1))
}

// enqueue queues a same-site page URL once.
func (c *crawler) enqueue(u *url.URL) {
	n := norm(u)
	if c.seen[n] {
		return
	}
	c.seen[n] = true
	c.queue = append(c.queue, n)
}

// classify routes a target a page links to.
func (c *crawler) classify(raw string) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return
	}
	switch {
	case !strings.EqualFold(u.Host, c.site.Host):
		c.extern = append(c.extern, norm(u))
	case c.sameSite(u) && pageLike(u) && c.allowed(u):
		c.enqueue(u)
	default:
		// An asset, a page outside the base path, or a page robots.txt
		// keeps the crawler out of: its status is still worth knowing.
		c.assets = append(c.assets, norm(u))
	}
}

func (c *crawler) pages(ctx context.Context) {
	root := *c.site
	c.enqueue(&root)
	for _, s := range c.seeds {
		if u, err := url.Parse(s); err == nil && c.allowed(u) {
			c.enqueue(u)
		}
	}
	fetched := 0
	for len(c.queue) > 0 && fetched < c.o.MaxPages {
		n := min(len(c.queue), c.o.Concurrency, c.o.MaxPages-fetched)
		batch := c.queue[:n]
		c.queue = c.queue[n:]
		resps := make([]Response, len(batch))
		var wg sync.WaitGroup
		for i, u := range batch {
			wg.Go(func() { resps[i] = c.fetch(ctx, http.MethodGet, u, maxPageBytes) })
		}
		wg.Wait()
		fetched += n
		for i, u := range batch {
			c.record(u, resps[i])
		}
	}
	if len(c.queue) > 0 {
		c.notes = append(c.notes, fmt.Sprintf("page cap reached: crawled %d pages; %d more were only status-checked", c.o.MaxPages, len(c.queue)))
		c.rest = c.queue
		c.queue = nil
	}
}

// record adds a fetched page to the input and queues what it links to.
func (c *crawler) record(raw string, r Response) {
	c.checked[raw] = true
	pu, _ := url.Parse(raw)
	if r.Status >= 300 && r.Status < 400 {
		c.in.Pages = append(c.in.Pages, siteaudit.Page{URL: raw, Status: r.Status, Header: r.Header})
		if loc := r.Header.Get("Location"); loc != "" {
			if lu, err := pu.Parse(loc); err == nil {
				c.classify(lu.String())
			}
		}
		return
	}
	if r.Status >= 200 && r.Status < 300 && !isHTML(r) {
		// A same-site URL that is not a page (a feed, a JSON file) is a
		// link target, not a page to judge.
		c.in.LinkResults = append(c.in.LinkResults, siteaudit.LinkResult{URL: raw, Status: r.Status})
		return
	}
	page := siteaudit.Page{URL: raw, Status: r.Status, Header: r.Header, Err: r.Err}
	if r.Status != 0 {
		page.RawHTML = string(r.Body)
	}
	c.in.Pages = append(c.in.Pages, page)
	if r.Status < 200 || r.Status >= 300 {
		return
	}
	for _, l := range siteaudit.DiscoverLinks(raw, page.RawHTML) {
		c.classify(l)
	}
	if img := strings.TrimSpace(siteaudit.ExtractHead(page.RawHTML).Social["og:image"]); img != "" {
		if iu, err := pu.Parse(img); err == nil && (iu.Scheme == "http" || iu.Scheme == "https") {
			c.images = append(c.images, norm(iu))
		}
	}
}

func isHTML(r Response) bool {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if ct == "" {
		return strings.Contains(strings.ToLower(http.DetectContentType(r.Body)), "html")
	}
	return strings.Contains(ct, "html")
}

// check is one status check to run.
type check struct {
	url   string
	image bool
}

func (c *crawler) linkChecks(ctx context.Context) {
	var todo []check
	add := func(u string, image bool, budget *int) bool {
		if c.checked[u] {
			return true
		}
		if *budget == 0 {
			return false
		}
		*budget--
		c.checked[u] = true
		todo = append(todo, check{url: u, image: image})
		return true
	}
	internal := c.o.MaxLinkChecks
	skipped := 0
	for _, u := range c.images {
		pu, _ := url.Parse(u)
		if !strings.EqualFold(pu.Host, c.site.Host) && !c.o.CheckExternal {
			continue
		}
		if !add(u, true, &internal) {
			skipped++
		}
	}
	for _, u := range append(append([]string{}, c.rest...), c.assets...) {
		if !add(u, false, &internal) {
			skipped++
		}
	}
	if skipped > 0 {
		c.notes = append(c.notes, fmt.Sprintf("link-check cap reached: %d internal targets were not checked", skipped))
	}
	switch {
	case c.o.CheckExternal:
		ext, extSkipped := c.o.MaxExternal, 0
		for _, u := range c.extern {
			if !add(u, false, &ext) {
				extSkipped++
			}
		}
		if extSkipped > 0 {
			c.notes = append(c.notes, fmt.Sprintf("external-link cap reached: %d external links were not checked", extSkipped))
		}
	case len(c.extern) > 0:
		c.notes = append(c.notes, fmt.Sprintf("%d external links were not checked", len(distinctStrings(c.extern))))
	}
	results := make([]siteaudit.LinkResult, len(todo))
	sem := make(chan struct{}, c.o.Concurrency)
	var wg sync.WaitGroup
	for i, ch := range todo {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = c.checkOne(ctx, ch)
		})
	}
	wg.Wait()
	c.in.LinkResults = append(c.in.LinkResults, results...)
}

// checkOne checks one target: HEAD, falling back to a capped GET when HEAD
// is refused or fails; an image is fetched whole, to test it for forge's
// placeholder. A redirect that stays on the target's own scheme and host is
// followed a few hops, so a target that moved is judged by where it landed;
// one to anywhere else is reported as the 3xx and never requested, so a
// link check reaches no destination the page did not name (the guarded
// client's dialer refuses private addresses besides).
func (c *crawler) checkOne(ctx context.Context, ch check) siteaudit.LinkResult {
	target := ch.url
	for hop := 0; ; hop++ {
		var r Response
		if ch.image {
			r = c.fetch(ctx, http.MethodGet, target, maxImageBytes)
		} else {
			r = c.fetch(ctx, http.MethodHead, target, 0)
			if r.Status == 0 || r.Status == http.StatusMethodNotAllowed || r.Status == http.StatusNotImplemented || r.Status == http.StatusForbidden {
				r = c.fetch(ctx, http.MethodGet, target, maxLinkGetBytes)
			}
		}
		if r.Status >= 300 && r.Status < 400 && hop < maxRedirectHops {
			if loc := r.Header.Get("Location"); loc != "" {
				base, _ := url.Parse(target)
				if next, err := base.Parse(loc); err == nil && next.Scheme == base.Scheme && strings.EqualFold(next.Host, base.Host) {
					target = next.String()
					continue
				}
			}
		}
		lr := siteaudit.LinkResult{URL: ch.url, Status: r.Status, Err: r.Err}
		if ch.image && r.Status >= 200 && r.Status < 300 {
			lr.Placeholder = siteaudit.IsPlaceholderImage(r.Body)
		}
		return lr
	}
}

func (c *crawler) notFound(ctx context.Context) {
	u := c.origin() + c.site.Path + NotFoundProbePath
	r := c.fetch(ctx, http.MethodGet, u, maxPageBytes)
	c.in.NotFound = &siteaudit.Page{URL: u, Status: r.Status, Header: r.Header, RawHTML: string(r.Body), Err: r.Err}
}

func (c *crawler) render(ctx context.Context) {
	if c.o.Renderer == nil {
		return
	}
	rendered, failed := 0, 0
	for i := range c.in.Pages {
		p := &c.in.Pages[i]
		if p.Status < 200 || p.Status >= 300 {
			continue
		}
		if rendered == c.o.MaxRender {
			c.notes = append(c.notes, fmt.Sprintf("render cap reached: rendered %d pages; the rest were judged on their raw HTML", c.o.MaxRender))
			break
		}
		rendered++
		target := p.URL
		if c.o.RenderURL != nil {
			target = c.o.RenderURL(p.URL)
		}
		html, axe, err := c.o.Renderer.Render(ctx, target)
		if err != nil {
			failed++
			if failed <= 5 {
				c.notes = append(c.notes, fmt.Sprintf("render failed for %s: %v", routeOf(mustParse(p.URL)), err))
			}
			continue
		}
		p.RenderedHTML, p.Axe = html, axe
	}
	if failed > 5 {
		c.notes = append(c.notes, fmt.Sprintf("render failed for %d more pages", failed-5))
	}
}

func mustParse(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		return &url.URL{Path: s}
	}
	return u
}

func distinctStrings(vs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vs {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
