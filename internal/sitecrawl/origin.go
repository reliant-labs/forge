package sitecrawl

import (
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// NotFoundFile is a site's not-found page at the root of its build. Its
// presence declares real 404s; without it the site is a single-page app.
const NotFoundFile = "404.html"

// staticExtensions are the extensions of files a static site serves as
// assets. A last path segment with one of them is a file, never a route, so
// it never gets the SPA fallback; any other segment is route-shaped
// (/releases/v1.8.3 is a route).
var staticExtensions = map[string]bool{
	".html": true, ".htm": true, ".js": true, ".mjs": true, ".cjs": true, ".css": true, ".map": true,
	".json": true, ".txt": true, ".xml": true, ".webmanifest": true, ".wasm": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true, ".ico": true, ".webp": true, ".avif": true, ".bmp": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".mp4": true, ".webm": true, ".mp3": true, ".ogg": true, ".wav": true,
	".pdf": true, ".zip": true, ".gz": true, ".csv": true, ".rss": true, ".atom": true,
}

// routeShaped reports whether a URL path names a route rather than a file.
func routeShaped(p string) bool {
	return !staticExtensions[strings.ToLower(path.Ext(p))]
}

// DirHandler serves the build directory dir mounted at basePath ("" or "/"
// for the host root, else "/docs"), with the hosted static origin's rules
// (see the package doc). It never lists a directory and refuses a path with a
// ".." segment.
//
// Every file is opened through one [os.Root] handle per request, and the
// bytes served are read from the descriptor that open returned: there is no
// check followed by a second open for a swapped symlink to slip between. A
// symlink that resolves inside the build is followed (a build may link one
// asset to another); one that resolves outside it — a file, or a directory —
// is not found, because the root refuses to open it.
func DirHandler(dir, basePath string) http.Handler {
	base := "/" + strings.Trim(basePath, "/")
	if base == "/" {
		base = ""
	}
	o := &dirOrigin{dir: dir, base: base}
	if root, err := os.OpenRoot(dir); err == nil {
		o.has404 = isFile(root, "/"+NotFoundFile)
		_ = root.Close()
	}
	return o
}

// serveFileHook, when set, runs after serveFile opens a file and before it
// reads it (tests swap the file there).
var serveFileHook func(rel string)

// ReadFile reads the build-relative file rel ("index.html") from dir
// without leaving it: a path or symlink that resolves outside dir fails.
func ReadFile(dir, rel string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(filepath.FromSlash(strings.TrimPrefix(rel, "/")))
}

type dirOrigin struct {
	dir    string
	base   string // "" or "/docs"
	has404 bool
}

func (o *dirOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reqPath := r.URL.Path
	for _, seg := range strings.Split(reqPath, "/") {
		if seg == ".." {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
	}
	if o.base != "" && reqPath != o.base && !strings.HasPrefix(reqPath, o.base+"/") {
		http.NotFound(w, r)
		return
	}
	if o.base != "" && reqPath == o.base {
		reqPath = o.base + "/"
	}
	rel := strings.TrimPrefix(reqPath, o.base) // starts with "/"
	root, err := os.OpenRoot(o.dir)
	if err != nil {
		http.Error(w, "the build directory cannot be read", http.StatusInternalServerError)
		return
	}
	defer func() { _ = root.Close() }()

	var names []string
	if strings.HasSuffix(rel, "/") {
		names = append(names, rel+"index.html")
	} else {
		names = append(names, rel)
		if routeShaped(rel) {
			names = append(names, rel+".html", rel+"/index.html")
		}
	}
	for _, n := range names {
		if serveFile(root, w, r, n, http.StatusOK) {
			return
		}
	}
	// Pretty URLs: /pricing/ names pricing.html when there is no
	// pricing/index.html, so it redirects to /pricing.
	if strings.HasSuffix(rel, "/") && rel != "/" {
		to := strings.TrimSuffix(rel, "/")
		if routeShaped(to) && isFile(root, to+".html") {
			loc := (&url.URL{Path: o.base + to}).EscapedPath()
			if r.URL.RawQuery != "" {
				loc += "?" + r.URL.RawQuery
			}
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
	}
	if o.has404 {
		if serveFile(root, w, r, "/"+NotFoundFile, http.StatusNotFound) {
			return
		}
	} else if routeShaped(rel) {
		if serveFile(root, w, r, "/index.html", http.StatusOK) {
			return
		}
	}
	http.NotFound(w, r)
}

// isFile reports a regular file at the build-relative path rel, inside the
// build. It decides only what to do (a redirect, the 404 mode); what is
// served is whatever serveFile's own open returns.
func isFile(root *os.Root, rel string) bool {
	fi, err := root.Stat(rootRel(rel))
	return err == nil && fi.Mode().IsRegular()
}

// rootRel is a build-relative URL path as an os.Root name.
func rootRel(rel string) string {
	if rel = strings.TrimPrefix(rel, "/"); rel == "" {
		return "."
	}
	return filepath.FromSlash(rel)
}

// serveFile writes the file at the build-relative path rel with status, and
// reports whether there was one. It opens the file once, through root, and
// judges and reads that same descriptor.
func serveFile(root *os.Root, w http.ResponseWriter, r *http.Request, rel string, status int) bool {
	f, err := root.Open(rootRel(rel))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if serveFileHook != nil {
		serveFileHook(rel)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	ct := mime.TypeByExtension(path.Ext(rel))
	switch {
	case strings.HasSuffix(rel, ".html"), strings.HasSuffix(rel, ".htm"):
		ct = "text/html; charset=utf-8"
	case ct == "":
		ct = http.DetectContentType(b)
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
	return true
}
