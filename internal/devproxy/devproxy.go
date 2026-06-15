// Package devproxy is the host-based reverse proxy that fronts every
// frontend and HTTP service spun up by `forge run`.
//
// Why host-based dispatch instead of path prefixes:
//
//   - Next.js path prefixes require `basePath` in next.config.js — a
//     file forge does NOT own (per project convention) and a config
//     that affects production routing too. Setting basePath JUST for
//     the dev loop is a foot-gun.
//   - HTTPRoute (Gateway API) already supports both shapes; the
//     production cluster will route by hostname (`admin.example.com`),
//     not path prefix. Doing the same in dev gives prod-parity for
//     free.
//
// Each dev process binds its own loopback port (the KCL-declared
// frontend.port / service.port). The proxy listens on a single port
// (default :8080) and dispatches by the request's `Host:` header,
// stripped of the trailing `:<port>`. `admin.localhost:8080` routes
// to whatever port `admin` is bound to; `web.localhost:8080` routes
// to `web`, etc. Browsers resolve `*.localhost` to 127.0.0.1 by RFC
// 6761, so no /etc/hosts edits are needed.
//
// WebSocket support is mandatory — Next.js HMR runs over a `_next/
// webpack-hmr` WS endpoint, and a proxy that drops Upgrade requests
// will silently break hot reload while leaving the rest of the dev
// loop apparently functional. The Hijacker-based splice in
// [Router.serveWebSocket] is the standard pattern for this; see
// https://github.com/golang/go/issues/26937 for the discussion.
package devproxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

// Backend is one routable target — a host header matched to a
// localhost backend port. Kind is informational only (banner output);
// the dispatch is host-only.
type Backend struct {
	// Host is the request-host suffix this entry matches. Stored
	// without `:port` (the proxy strips that before lookup). e.g.
	// "admin.localhost".
	Host string
	// Port is the loopback TCP port the dev process listens on.
	Port int
	// Kind is "frontend" or "service" — used only in the banner.
	Kind string
	// Name is the project-declared component name. Banner output.
	Name string
}

// Router dispatches by Host header to a per-backend reverse proxy.
// Backends are immutable after [New]; the cached proxy map is built
// once at construction.
type Router struct {
	backends []Backend
	// byHost is the dispatch table — request host (sans port) → backend
	// index in `backends`.
	byHost map[string]int
	// proxies is the per-backend httputil.ReverseProxy, keyed by index.
	// Built once at construction so we don't pay url.Parse + Director
	// allocation on every request.
	proxies []*httputil.ReverseProxy
	// defaultIdx is the backend to fall through to when the request
	// host doesn't match any declared backend. -1 means "404 on
	// unmatched host". Set to the first frontend at construction so a
	// bare `http://localhost:8080/` lands somewhere useful in the
	// common single-frontend case.
	defaultIdx int
}

// New builds a Router from the given backends. The first backend with
// Kind=="frontend" (preserving the input order) becomes the default
// for unmatched hosts; if no frontends are present, unmatched hosts
// 404.
func New(backends []Backend) *Router {
	r := &Router{
		backends:   backends,
		byHost:     make(map[string]int, len(backends)),
		proxies:    make([]*httputil.ReverseProxy, len(backends)),
		defaultIdx: -1,
	}
	for i, b := range backends {
		host := normalizeHost(b.Host)
		r.byHost[host] = i
		target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", b.Port))
		r.proxies[i] = httputil.NewSingleHostReverseProxy(target)
		// Quiet the default error log — the orchestrator owns
		// stdout; backend-down errors surface as 502s.
		r.proxies[i].ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
			http.Error(w, fmt.Sprintf("[devproxy] backend %s (port %d) error: %v", b.Name, b.Port, err), http.StatusBadGateway)
		}
		if r.defaultIdx < 0 && b.Kind == "frontend" {
			r.defaultIdx = i
		}
	}
	return r
}

// Backends returns the configured backends in declaration order. Used
// by the banner printer in run.go.
func (r *Router) Backends() []Backend {
	out := make([]Backend, len(r.backends))
	copy(out, r.backends)
	return out
}

// ServeHTTP dispatches by Host header. The path goes to the matched
// backend untouched (no rewrite) — frontends and services see exactly
// what the browser sent.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	idx, ok := r.lookup(req.Host)
	if !ok {
		if r.defaultIdx < 0 {
			http.Error(w, fmt.Sprintf("[devproxy] no route for host %q", req.Host), http.StatusNotFound)
			return
		}
		idx = r.defaultIdx
	}

	// WebSocket upgrade is hijacked + spliced manually; the stdlib
	// ReverseProxy added WS support in Go 1.12 but it's quirky around
	// the Hijacker interface, and the manual path is ~50 LOC of pure
	// io.Copy that's easier to reason about for HMR.
	if isWebSocketUpgrade(req) {
		r.serveWebSocket(w, req, r.backends[idx])
		return
	}

	r.proxies[idx].ServeHTTP(w, req)
}

// lookup maps a request Host header (with or without `:port`) to a
// backend index.
func (r *Router) lookup(host string) (int, bool) {
	h := normalizeHost(host)
	idx, ok := r.byHost[h]
	return idx, ok
}

// normalizeHost strips the trailing port (if any) and lowercases. The
// stdlib's net.SplitHostPort returns an error when there's no port,
// so we handle the no-port case ourselves.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// isWebSocketUpgrade returns true when the request is asking to
// upgrade to WebSocket. Header values are case-insensitive per RFC
// 7230 §3.2, and Connection can be a comma-separated list.
func isWebSocketUpgrade(req *http.Request) bool {
	if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for tok := range strings.SplitSeq(req.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
			return true
		}
	}
	return false
}

// serveWebSocket hijacks the client connection, dials the backend,
// replays the original HTTP request (so the server completes its
// 101 handshake), and splices the two sockets together.
//
// The canonical shape (see golang/go#26937 and friends): clone the
// request preserving Host, dial TCP to the backend, write the request
// bytes via http.Request.Write, then io.Copy in both directions until
// either side closes.
func (r *Router) serveWebSocket(w http.ResponseWriter, req *http.Request, b Backend) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "[devproxy] websocket: response writer does not support hijacking", http.StatusInternalServerError)
		return
	}

	backendAddr := fmt.Sprintf("127.0.0.1:%d", b.Port)
	backendConn, err := net.Dial("tcp", backendAddr)
	if err != nil {
		http.Error(w, fmt.Sprintf("[devproxy] websocket: dial backend %s: %v", backendAddr, err), http.StatusBadGateway)
		return
	}
	defer backendConn.Close()

	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		// Best-effort: the client is already toast.
		http.Error(w, fmt.Sprintf("[devproxy] websocket: hijack: %v", err), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Replay the original request to the backend. We deep-copy
	// req.URL and clear the absolute-form fields so http.Request.Write
	// emits the origin-form (`GET /path HTTP/1.1`) that any reasonable
	// HTTP server expects. Host stays whatever the client sent so the
	// backend's vhost matching still works.
	outReq := req.Clone(req.Context())
	outReq.URL = &url.URL{
		Path:     req.URL.Path,
		RawQuery: req.URL.RawQuery,
	}
	outReq.RequestURI = ""

	if err := outReq.Write(backendConn); err != nil {
		_, _ = clientBuf.WriteString("HTTP/1.1 502 Bad Gateway\r\n\r\n")
		_ = clientBuf.Flush()
		return
	}

	// Splice both directions. The first side to error or EOF causes
	// the goroutine to close its half-connection (via io.Copy on a TCP
	// conn returning); waitgroup ensures we don't return until both
	// directions have unwound.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(backendConn, clientBuf)
		// Half-close the backend write side so the backend's read
		// loop sees EOF and tears down.
		if tcp, ok := backendConn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, backendConn)
		if tcp, ok := clientConn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	wg.Wait()
}
