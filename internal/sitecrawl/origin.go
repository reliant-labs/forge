package sitecrawl

import (
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
// Every file is read through an [os.Root] at dir, so nothing outside the
// build is ever served: a symlink that resolves inside the build is followed
// (a build may link one asset to another), and one that resolves outside it
// — a file, or a directory — is not found.
func DirHandler(dir, basePath string) http.Handler {
	base := "/" + strings.Trim(basePath, "/")
	if base == "/" {
		base = ""
	}
	o := &dirOrigin{dir: dir, base: base}
	o.has404 = o.isFile("/" + NotFoundFile)
	return o
}

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
		if o.serveFile(w, r, n, http.StatusOK) {
			return
		}
	}
	// Pretty URLs: /pricing/ names pricing.html when there is no
	// pricing/index.html, so it redirects to /pricing.
	if strings.HasSuffix(rel, "/") && rel != "/" {
		to := strings.TrimSuffix(rel, "/")
		if routeShaped(to) && o.isFile(to+".html") {
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
		if o.serveFile(w, r, "/"+NotFoundFile, http.StatusNotFound) {
			return
		}
	} else if routeShaped(rel) {
		if o.serveFile(w, r, "/index.html", http.StatusOK) {
			return
		}
	}
	http.NotFound(w, r)
}

// isFile reports a regular file at the build-relative path rel, inside the
// build.
func (o *dirOrigin) isFile(rel string) bool {
	root, err := os.OpenRoot(o.dir)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Stat(filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	return err == nil && fi.Mode().IsRegular()
}

// serveFile writes the file at the build-relative path rel with status, and
// reports whether there was one.
func (o *dirOrigin) serveFile(w http.ResponseWriter, r *http.Request, rel string, status int) bool {
	if !o.isFile(rel) {
		return false
	}
	b, err := ReadFile(o.dir, rel)
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
