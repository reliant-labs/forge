package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/reliant-labs/forge/pkg/netguard"
)

// THE BOUNDARY. A page the render pass loads runs its own JavaScript in
// Chrome on the user's machine, and that script can request any URL: an
// admin page on localhost, a router on 192.168.0.1, the cloud metadata
// service at 169.254.169.254. Guarding the crawler's Go client does nothing
// for Chrome's network stack, so Chrome is started with this proxy as its
// only way out (--proxy-server, --proxy-bypass-list=<-loopback> so loopback
// goes through it too, and non-proxied UDP disabled so WebRTC cannot go
// around it). The proxy dials every destination through pkg/netguard, which
// checks the address the socket is about to connect to, after DNS: a
// private, loopback, link-local or metadata address is refused however the
// page reached it — a subresource, a fetch(), a redirect (Chrome follows a
// redirect by asking the proxy again; the proxy itself follows none), a
// rebinding DNS name. HTTPS arrives as CONNECT and is tunnelled, not
// terminated: the destination's address is what the guard judges, so the
// proxy needs no certificate of its own.
//
// A directory audit serves the build on loopback for Chrome; AllowAddr
// admits exactly that address (host:port as Chrome writes it), nothing else
// private. (control-plane's site auditor, internal/siteauditor/render, is
// the same boundary on the platform, where it also signs and filters
// requests.)

// proxyDialTimeout bounds one upstream connection attempt.
const proxyDialTimeout = 10 * time.Second

// ProxyOptions configure a [Proxy].
type ProxyOptions struct {
	// Ports are the destination ports a public address may be reached on;
	// empty means 80 and 443.
	Ports []uint16
	// AllowAddr is the one host:port admitted though it is private (the
	// loopback server a directory audit serves the build from); "" admits
	// none.
	AllowAddr string
}

// Proxy is the render pass's forward proxy, listening on loopback.
type Proxy struct {
	ln      net.Listener
	srv     *http.Server
	rt      *http.Transport
	allow   string
	guarded func(ctx context.Context, network, addr string) (net.Conn, error)
	refused atomic.Int64

	mu      sync.Mutex
	tunnels map[net.Conn]struct{}
	closed  bool
}

// StartProxy starts a proxy on a loopback port.
func StartProxy(o ProxyOptions) (*Proxy, error) {
	ports := o.Ports
	if len(ports) == 0 {
		ports = []uint16{80, 443}
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("the render proxy's listener: %w", err)
	}
	p := &Proxy{
		ln:      ln,
		allow:   strings.ToLower(o.AllowAddr),
		guarded: netguard.Policy{Ports: ports}.DialContext(&net.Dialer{Timeout: proxyDialTimeout}),
		tunnels: map[net.Conn]struct{}{},
	}
	p.rt = &http.Transport{
		Proxy:                 nil,
		DialContext:           p.dial,
		DisableCompression:    true, // the browser's Accept-Encoding goes upstream as sent
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// URL is the proxy's address, as Chrome's --proxy-server takes it.
func (p *Proxy) URL() string { return "http://" + p.ln.Addr().String() }

// Refused counts the connections the guard refused.
func (p *Proxy) Refused() int64 { return p.refused.Load() }

// Close stops the proxy and every tunnel through it.
func (p *Proxy) Close() {
	p.mu.Lock()
	p.closed = true
	for c := range p.tunnels {
		_ = c.Close()
	}
	p.mu.Unlock()
	_ = p.srv.Close()
	p.rt.CloseIdleConnections()
}

// dial connects to addr: the admitted loopback address directly, anything
// else through the guard.
func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.allow != "" && strings.EqualFold(addr, p.allow) {
		return (&net.Dialer{Timeout: proxyDialTimeout}).DialContext(ctx, network, addr)
	}
	c, err := p.guarded(ctx, network, addr)
	if err != nil && errors.Is(err, netguard.ErrRefused) {
		p.refused.Add(1)
	}
	return c, err
}

// fail answers a request the proxy could not forward: 403 for a refused
// destination, 502 for one that failed.
func fail(w http.ResponseWriter, err error) {
	if errors.Is(err, netguard.ErrRefused) {
		http.Error(w, "forge site audit's render proxy refused the destination: "+err.Error(), http.StatusForbidden)
		return
	}
	http.Error(w, "forge site audit's render proxy could not reach the destination: "+err.Error(), http.StatusBadGateway)
}

// hopHeaders are the hop-by-hop headers a proxy drops.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// ServeHTTP is the proxy's front door: CONNECT opens a tunnel, an
// absolute-form http:// request is forwarded, anything else is refused.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if !r.URL.IsAbs() || r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "the render proxy forwards absolute http:// requests and CONNECT only", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	resp, err := p.rt.RoundTrip(out) // follows no redirect: Chrome asks again, through here
	if err != nil {
		fail(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, h := range hopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// connect tunnels a CONNECT to its destination, dialled through the guard.
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), proxyDialTimeout)
	upstream, err := p.dial(ctx, "tcp", r.Host)
	cancel()
	if err != nil {
		fail(w, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "the render proxy cannot tunnel this connection", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if !p.track(client, upstream) {
		return
	}
	defer p.untrack(client, upstream)
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		if n := buf.Reader.Buffered(); n > 0 {
			b, _ := buf.Reader.Peek(n)
			_, _ = upstream.Write(b)
		}
		_, _ = io.Copy(upstream, client)
		done <- struct{}{}
	}()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

func (p *Proxy) track(cs ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		for _, c := range cs {
			_ = c.Close()
		}
		return false
	}
	for _, c := range cs {
		p.tunnels[c] = struct{}{}
	}
	return true
}

func (p *Proxy) untrack(cs ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
		delete(p.tunnels, c)
	}
}
