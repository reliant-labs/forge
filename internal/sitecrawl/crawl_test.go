package sitecrawl

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/siteaudit"
)

// writeSite writes files (build-relative path → content) under a temp dir.
func writeSite(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func page(title, body string) string {
	return `<!doctype html><html lang="en"><head><title>` + title + `</title></head><body>` + body + `</body></html>`
}

func TestDirHandlerServesLikeHostedStaticHosting(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"index.html":      page("Home", "home"),
		"about.html":      page("About", "about"),
		"docs/index.html": page("Docs", "docs"),
		"app.js":          "console.log(1)",
	}
	spa := writeSite(t, files)
	files["404.html"] = page("Not found", "nf")
	export := writeSite(t, files)

	cases := []struct {
		name, dir, base, path string
		status                int
		body, location        string
	}{
		{"root", spa, "", "/", 200, "home", ""},
		{"x.html", spa, "", "/about", 200, "about", ""},
		{"x/index.html", spa, "", "/docs", 200, "docs", ""},
		{"dir slash", spa, "", "/docs/", 200, "docs", ""},
		{"pretty url redirect", spa, "", "/about/", 301, "", "/about"},
		{"asset", spa, "", "/app.js", 200, "console.log", ""},
		{"missing asset never gets the shell", spa, "", "/gone.js", 404, "", ""},
		{"spa fallback", spa, "", "/some/route", 200, "home", ""},
		{"404.html declares real 404s", export, "", "/some/route", 404, "nf", ""},
		{"traversal refused", spa, "", "/../etc/passwd", 400, "", ""},
		{"base path root", spa, "/docs-site", "/docs-site/", 200, "home", ""},
		{"base path page", spa, "/docs-site", "/docs-site/about", 200, "about", ""},
		{"outside the mount", spa, "/docs-site", "/about", 404, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := DirHandler(tc.dir, tc.base)
			req := httptest.NewRequest(http.MethodGet, "http://site.test/", nil)
			req.URL.Path = tc.path
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("GET %s: status %d, want %d", tc.path, rec.Code, tc.status)
			}
			if tc.body != "" && !strings.Contains(rec.Body.String(), tc.body) {
				t.Errorf("GET %s: body %q, want it to contain %q", tc.path, rec.Body.String(), tc.body)
			}
			if got := rec.Header().Get("Location"); got != tc.location {
				t.Errorf("GET %s: Location %q, want %q", tc.path, got, tc.location)
			}
		})
	}
}

func pageURLs(in siteaudit.Input) []string {
	var out []string
	for _, p := range in.Pages {
		out = append(out, p.URL)
	}
	slices.Sort(out)
	return out
}

func linkStatus(in siteaudit.Input, u string) (siteaudit.LinkResult, bool) {
	for _, lr := range in.LinkResults {
		if lr.URL == u {
			return lr, true
		}
	}
	return siteaudit.LinkResult{}, false
}

func TestCrawlLocalBuild(t *testing.T) {
	t.Parallel()
	dir := writeSite(t, map[string]string{
		"index.html": page("Home", `<a href="/about">About</a> <a href="https://elsewhere.example/x">out</a>
			<a href="/private/secret">secret</a> <a href="/files/missing.pdf">pdf</a> <a href="/about#team">team</a>`),
		"about.html": page("About", `<a href="/">home</a>`),
		"deep.html":  page("Deep", "only the sitemap knows me"),
		"robots.txt": "User-agent: *\nDisallow: /private/\n\nSitemap: https://acme.example/sitemap.xml\n",
		"sitemap.xml": `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
			<url><loc>https://acme.example/</loc></url><url><loc>https://acme.example/deep</loc></url></urlset>`,
		"404.html": page("Not found", "nf"),
	})
	res, err := Crawl(context.Background(), Options{
		Site:    "https://acme.example",
		Fetcher: HandlerFetcher{Host: "acme.example", Handler: DirHandler(dir, "")},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := res.Input
	if in.Site != "https://acme.example/" {
		t.Errorf("Site = %q", in.Site)
	}
	want := []string{"https://acme.example/", "https://acme.example/about", "https://acme.example/deep"}
	if got := pageURLs(in); !slices.Equal(got, want) {
		t.Errorf("pages = %v, want %v (robots-disallowed /private/ is not fetched as a page; the sitemap seeds /deep)", got, want)
	}
	if in.Robots == nil || in.Robots.Status != 200 {
		t.Errorf("robots = %+v", in.Robots)
	}
	if len(in.Sitemaps) != 1 || in.Sitemaps[0].Status != 200 {
		t.Errorf("sitemaps = %+v", in.Sitemaps)
	}
	if lr, ok := linkStatus(in, "https://acme.example/files/missing.pdf"); !ok || lr.Status != 404 {
		t.Errorf("missing.pdf check = %+v, %v; want a 404 status check", lr, ok)
	}
	if lr, ok := linkStatus(in, "https://acme.example/private/secret"); !ok || lr.Status != 404 {
		t.Errorf("robots-blocked page = %+v, %v; want it status-checked, not crawled", lr, ok)
	}
	if _, ok := linkStatus(in, "https://elsewhere.example/x"); ok {
		t.Error("an external link was checked without CheckExternal")
	}
	if !slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, "1 external links were not checked") }) {
		t.Errorf("notes = %v, want the unchecked external link named", res.Notes)
	}
	if in.NotFound == nil || in.NotFound.Status != 404 {
		t.Errorf("not-found probe = %+v, want the 404.html answer", in.NotFound)
	}
	rep := siteaudit.Evaluate(in)
	if rep.Site.NotFound != "not_found" {
		t.Errorf("site not_found = %q", rep.Site.NotFound)
	}
}

func TestCrawlFollowsNoRedirectAndReadsGzipSitemaps(t *testing.T) {
	t.Parallel()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			_, _ = w.Write([]byte("User-agent: *\nAllow: /\nSitemap: " + srv.URL + "/sitemap.xml.gz\n"))
		case "/sitemap.xml.gz":
			w.Header().Set("Content-Type", "application/gzip")
			var gz bytes.Buffer
			zw := gzip.NewWriter(&gz)
			_, _ = zw.Write([]byte(`<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>` + srv.URL + `/listed</loc></url></urlset>`))
			_ = zw.Close()
			_, _ = w.Write(gz.Bytes())
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(page("Home", `<a href="/moved">moved</a>`)))
		case "/moved":
			http.Redirect(w, r, "/new-home", http.StatusMovedPermanently)
		case "/new-home", "/listed":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(page("Page", "")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := Crawl(context.Background(), Options{Site: srv.URL, Fetcher: HTTPFetcher{Client: client}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, p := range res.Input.Pages {
		got[strings.TrimPrefix(p.URL, srv.URL)] = p.Status
	}
	for path, status := range map[string]int{"/": 200, "/moved": 301, "/new-home": 200, "/listed": 200} {
		if got[path] != status {
			t.Errorf("page %s: status %d, want %d (all pages: %v)", path, got[path], status, got)
		}
	}
	if len(res.Input.Sitemaps) != 1 || !strings.Contains(res.Input.Sitemaps[0].Body, "<urlset") {
		t.Errorf("sitemaps = %+v, want the gzipped sitemap decompressed", res.Input.Sitemaps)
	}
}

func TestCrawlPageCapStatusChecksTheRest(t *testing.T) {
	t.Parallel()
	dir := writeSite(t, map[string]string{
		"index.html": page("Home", `<a href="/a">a</a><a href="/b">b</a><a href="/c">c</a>`),
		"a.html":     page("A", ""),
		"b.html":     page("B", ""),
		"c.html":     page("C", ""),
	})
	res, err := Crawl(context.Background(), Options{
		Site:     "https://acme.example/",
		Fetcher:  HandlerFetcher{Host: "acme.example", Handler: DirHandler(dir, "")},
		MaxPages: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Input.Pages); n != 2 {
		t.Errorf("crawled %d pages, want 2", n)
	}
	for _, u := range []string{"https://acme.example/b", "https://acme.example/c"} {
		if lr, ok := linkStatus(res.Input, u); !ok || lr.Status != 200 {
			t.Errorf("%s: %+v, %v; want it status-checked", u, lr, ok)
		}
	}
	if !slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, "page cap reached") }) {
		t.Errorf("notes = %v", res.Notes)
	}
}

func TestCrawlFlagsThePlaceholderOGImage(t *testing.T) {
	t.Parallel()
	dir := writeSite(t, map[string]string{
		"index.html": `<html><head><meta property="og:image" content="https://acme.example/og.png"></head><body></body></html>`,
		"og.png":     "\x89PNG\r\n\x1a\n tEXt " + siteaudit.PlaceholderImageMarker,
	})
	res, err := Crawl(context.Background(), Options{
		Site:    "https://acme.example/",
		Fetcher: HandlerFetcher{Host: "acme.example", Handler: DirHandler(dir, "")},
	})
	if err != nil {
		t.Fatal(err)
	}
	lr, ok := linkStatus(res.Input, "https://acme.example/og.png")
	if !ok || lr.Status != 200 || !lr.Placeholder {
		t.Errorf("og.png check = %+v, %v; want a 200 flagged as the placeholder", lr, ok)
	}
}

func TestCrawlRefusesARelativeSite(t *testing.T) {
	t.Parallel()
	if _, err := Crawl(context.Background(), Options{Site: "/just/a/path", Fetcher: HTTPFetcher{Client: http.DefaultClient}}); err == nil {
		t.Fatal("want an error for a site that is not an absolute http(s) URL")
	}
}

// recordingFetcher answers from a table and records every URL requested.
type recordingFetcher struct {
	mu        sync.Mutex
	responses map[string]Response
	requested []string
}

func (f *recordingFetcher) Fetch(_ context.Context, _, rawURL string, _ int64) Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requested = append(f.requested, rawURL)
	if r, ok := f.responses[rawURL]; ok {
		return r
	}
	return Response{Status: 404, Header: http.Header{}}
}

func TestLinkChecksFollowOnlySameHostRedirects(t *testing.T) {
	t.Parallel()
	html := http.Header{"Content-Type": {"text/html"}}
	redirect := func(to string) Response {
		return Response{Status: 302, Header: http.Header{"Location": {to}}}
	}
	f := &recordingFetcher{responses: map[string]Response{
		"https://acme.example/": {Status: 200, Header: html, Body: []byte(page("Home",
			`<a href="https://public.example/go">out</a> <a href="https://acme.example/old.pdf">pdf</a>`))},
		"https://public.example/go":    redirect("http://127.0.0.1:8080/admin"),
		"https://acme.example/old.pdf": redirect("/new.pdf"),
		"https://acme.example/new.pdf": {Status: 200, Header: http.Header{}},
	}}
	res, err := Crawl(context.Background(), Options{Site: "https://acme.example/", Fetcher: f, CheckExternal: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range f.requested {
		if strings.Contains(u, "127.0.0.1") {
			t.Fatalf("a link check followed a redirect off the linked host to %s (requested: %v)", u, f.requested)
		}
	}
	if lr, ok := linkStatus(res.Input, "https://public.example/go"); !ok || lr.Status != 302 {
		t.Errorf("cross-host redirect = %+v, %v; want the 302 reported", lr, ok)
	}
	if lr, ok := linkStatus(res.Input, "https://acme.example/old.pdf"); !ok || lr.Status != 200 {
		t.Errorf("same-host redirect = %+v, %v; want it followed to the 200", lr, ok)
	}
}

func TestGuardedClientRefusesLoopback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the guarded client reached a loopback server")
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	r := HTTPFetcher{Client: GuardedClient(u, 5*time.Second)}.Fetch(context.Background(), http.MethodGet, srv.URL, 1024)
	if r.Status != 0 || !strings.Contains(r.Err, "refused") {
		t.Fatalf("fetch of %s = status %d err %q; want a netguard refusal", srv.URL, r.Status, r.Err)
	}
	if err := RefusedHost(u); err == nil {
		t.Error("RefusedHost accepted a loopback address")
	}
}
