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
	"runtime"
	"slices"
	"strconv"
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

// The build is served through an os.Root: a symlink that escapes the build
// (to a file or a directory) is not found, one that stays inside it works.
func TestDirHandlerNeverServesOutsideTheBuild(t *testing.T) {
	t.Parallel()
	outside := writeSite(t, map[string]string{"secret.txt": "TOP SECRET", "etc/passwd": "root:x:0:0", "index.html": page("Outside", "outside")})
	dir := writeSite(t, map[string]string{
		"index.html":      page("Home", "home"),
		"assets/real.css": "body{}",
		"docs/guide.html": page("Guide", "guide"),
	})
	links := map[string]string{
		"leak.txt":       filepath.Join(outside, "secret.txt"),
		"etc":            filepath.Join(outside, "etc"),
		"escape":         outside,
		"rel-escape.txt": "../" + filepath.Base(outside) + "/secret.txt",
		"alias.css":      filepath.Join("assets", "real.css"),
		"manual":         "docs",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Skipf("symlinks unavailable here: %v", err)
		}
	}
	h := DirHandler(dir, "")
	get := func(p string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://site.test/", nil)
		req.URL.Path = p
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, p := range []string{"/leak.txt", "/etc/passwd", "/escape/secret.txt", "/escape/", "/escape", "/rel-escape.txt"} {
		rec := get(p)
		if body := rec.Body.String(); strings.Contains(body, "SECRET") || strings.Contains(body, "root:x") || strings.Contains(body, "outside") {
			t.Errorf("GET %s served a file outside the build (status %d): %q", p, rec.Code, body)
		}
	}
	if rec := get("/leak.txt"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /leak.txt: status %d, want 404", rec.Code)
	}
	if rec := get("/alias.css"); rec.Code != 200 || rec.Body.String() != "body{}" {
		t.Errorf("an in-build symlinked file: status %d body %q", rec.Code, rec.Body.String())
	}
	if rec := get("/manual/guide"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "guide") {
		t.Errorf("an in-build symlinked directory: status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestReadFileStaysInsideTheBuild(t *testing.T) {
	t.Parallel()
	outside := writeSite(t, map[string]string{"secret.txt": "TOP SECRET"})
	dir := writeSite(t, map[string]string{"assets/real.css": "body{}"})
	for name, target := range map[string]string{"leak.txt": filepath.Join(outside, "secret.txt"), "escape": outside, "alias.css": filepath.Join("assets", "real.css")} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Skipf("symlinks unavailable here: %v", err)
		}
	}
	for _, rel := range []string{"leak.txt", "escape/secret.txt", "../" + filepath.Base(outside) + "/secret.txt"} {
		if b, err := ReadFile(dir, rel); err == nil {
			t.Errorf("ReadFile(%s) read outside the build: %q", rel, b)
		}
	}
	if b, err := ReadFile(dir, "alias.css"); err != nil || string(b) != "body{}" {
		t.Errorf("ReadFile of an in-build symlink = %q, %v", b, err)
	}
}

// TestDirHandlerSymlinkSwapAfterOpenServesNothingOutside swaps the file
// (and, separately, its directory) for a symlink out of the build after the
// handler opened it: what is served is read from the descriptor it opened,
// never from what the name points at now. Not parallel: it sets a package
// hook.
func TestDirHandlerSymlinkSwapAfterOpenServesNothingOutside(t *testing.T) {
	outside := writeSite(t, map[string]string{"page.html": "OUTSIDE SECRET", "docs/guide.html": "OUTSIDE SECRET"})
	for name, swap := range map[string]struct{ reqPath, replace, target string }{
		"file":      {"/page.html", "page.html", filepath.Join(outside, "page.html")},
		"directory": {"/docs/guide.html", "docs", filepath.Join(outside, "docs")},
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeSite(t, map[string]string{"index.html": page("Home", "home"), "page.html": "original", "docs/guide.html": "original"})
			swapped := false
			serveFileHook = func(rel string) {
				if rel != swap.reqPath || swapped {
					return
				}
				swapped = true
				p := filepath.Join(dir, swap.replace)
				if err := os.RemoveAll(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(swap.target, p); err != nil {
					t.Skipf("symlinks unavailable here: %v", err)
				}
			}
			t.Cleanup(func() { serveFileHook = nil })
			req := httptest.NewRequest(http.MethodGet, "http://site.test/", nil)
			req.URL.Path = swap.reqPath
			rec := httptest.NewRecorder()
			DirHandler(dir, "").ServeHTTP(rec, req)
			if !swapped {
				t.Fatal("the hook never ran: the handler did not open the file")
			}
			if body := rec.Body.String(); strings.Contains(body, "SECRET") || body != "original" {
				t.Errorf("GET %s after the swap: status %d body %q; want the opened file's bytes", swap.reqPath, rec.Code, body)
			}
			// The next request sees the swap, and the root refuses it.
			rec = httptest.NewRecorder()
			DirHandler(dir, "").ServeHTTP(rec, req)
			if strings.Contains(rec.Body.String(), "SECRET") {
				t.Errorf("GET %s once swapped served the outside file", swap.reqPath)
			}
		})
	}
}

// TestHandlerFetchOfAHugeFileStaysBounded fetches a 50 MiB (sparse) asset
// from a build through the in-process handler with a 1 MiB limit: the body
// is cut at the limit, marked truncated, and the fetch allocates a bounded
// amount — the file is streamed and the copy stops at the cap, never read
// or buffered whole. Not parallel: it reads the process's allocation total.
func TestHandlerFetchOfAHugeFileStaysBounded(t *testing.T) {
	dir := writeSite(t, map[string]string{"index.html": page("Home", "home")})
	f, err := os.Create(filepath.Join(dir, "huge.bin"))
	if err != nil {
		t.Fatal(err)
	}
	const size, limit = 50 << 20, 1 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	fetcher := HandlerFetcher{Host: "acme.example", Handler: DirHandler(dir, "")}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := fetcher.Fetch(context.Background(), http.MethodGet, "https://acme.example/huge.bin", limit)
	runtime.ReadMemStats(&after)

	if r.Status != http.StatusOK || len(r.Body) != limit || !r.Truncated {
		t.Fatalf("fetch = status %d, %d bytes, truncated %v; want 200, %d, true", r.Status, len(r.Body), r.Truncated, limit)
	}
	if got := r.Header.Get("Content-Length"); got != strconv.Itoa(size) {
		t.Errorf("Content-Length = %q, want %d", got, size)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 8<<20 {
		t.Errorf("the fetch allocated %d MiB for a %d MiB limit: the file was read or buffered whole", alloc>>20, limit>>20)
	}
	small := fetcher.Fetch(context.Background(), http.MethodGet, "https://acme.example/", limit)
	if small.Truncated || !strings.Contains(string(small.Body), "home") {
		t.Errorf("a page under the limit: truncated %v body %q", small.Truncated, small.Body)
	}
}

// A link a page names with credentials in it is requested without them, and
// no request carries an Authorization header.
func TestCrawlNeverSendsUserinfo(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			host := strings.TrimPrefix(srv.URL, "http://")
			_, _ = w.Write([]byte(page("Home", `<a href="http://admin:hunter2@`+host+`/private">p</a>`)))
			return
		}
		_, _ = w.Write([]byte(page("Page", "")))
	}))
	t.Cleanup(srv.Close)
	res, err := Crawl(context.Background(), Options{Site: srv.URL, Fetcher: HTTPFetcher{Client: http.DefaultClient}})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	reached := false
	for _, s := range seen {
		if !strings.HasSuffix(s, " auth=") {
			t.Errorf("a request carried credentials: %s", s)
		}
		reached = reached || strings.HasPrefix(s, "/private ")
	}
	if !reached {
		t.Errorf("the discovered link was not crawled (requests: %v)", seen)
	}
	for _, p := range res.Input.Pages {
		if strings.Contains(p.URL, "hunter2") {
			t.Errorf("a page URL kept the credentials: %s", p.URL)
		}
	}
	if _, err := Crawl(context.Background(), Options{Site: "https://user:pass@public.example/", Fetcher: HTTPFetcher{Client: http.DefaultClient}}); err == nil {
		t.Error("Crawl accepted a site URL with credentials")
	}
}
