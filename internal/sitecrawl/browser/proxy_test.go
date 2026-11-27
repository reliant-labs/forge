package browser

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingServer is a loopback server that counts the requests it serves.
func countingServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if h != nil {
			h(w, r)
			return
		}
		_, _ = w.Write([]byte("reached"))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func startTestProxy(t *testing.T, o ProxyOptions) *Proxy {
	t.Helper()
	p, err := StartProxy(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// viaProxy is a client that sends everything through p and follows
// redirects (each hop goes through p again, as Chrome's do).
func viaProxy(t *testing.T, p *Proxy) *http.Client {
	t.Helper()
	pu, _ := url.Parse(p.URL())
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
}

func TestProxyRefusesPrivateDestinations(t *testing.T) {
	t.Parallel()
	target, hits := countingServer(t, nil)
	p := startTestProxy(t, ProxyOptions{})
	c := viaProxy(t, p)
	for _, u := range []string{target.URL + "/admin", "http://169.254.169.254/latest/meta-data/", "http://[::1]:9/"} {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s through the proxy: status %d, want 403", u, resp.StatusCode)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("the loopback server was reached %d times through the proxy", hits.Load())
	}
	if p.Refused() < 3 {
		t.Errorf("Refused = %d, want every private destination counted", p.Refused())
	}
}

func TestProxyRefusesACONNECTToAPrivateAddress(t *testing.T) {
	t.Parallel()
	target, hits := countingServer(t, nil)
	p := startTestProxy(t, ProxyOptions{Ports: []uint16{443}})
	conn, err := net.Dial("tcp", strings.TrimPrefix(p.URL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	host := strings.TrimPrefix(target.URL, "http://")
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("CONNECT %s: status %d, want 403", host, resp.StatusCode)
	}
	if hits.Load() != 0 {
		t.Error("the loopback server was reached through a CONNECT tunnel")
	}
}

func TestProxyAdmitsOnlyTheDirectoryAuditsLoopbackServer(t *testing.T) {
	t.Parallel()
	build, buildHits := countingServer(t, nil)
	other, otherHits := countingServer(t, nil)
	p := startTestProxy(t, ProxyOptions{AllowAddr: strings.TrimPrefix(build.URL, "http://")})
	c := viaProxy(t, p)
	resp, err := c.Get(build.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || buildHits.Load() != 1 {
		t.Errorf("the admitted build server: status %d, hits %d; want 200, 1", resp.StatusCode, buildHits.Load())
	}
	resp, err = c.Get(other.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || otherHits.Load() != 0 {
		t.Errorf("another loopback server: status %d, hits %d; want 403, 0", resp.StatusCode, otherHits.Load())
	}
}

func TestProxyRefusesARedirectToAPrivateAddress(t *testing.T) {
	t.Parallel()
	internal, internalHits := countingServer(t, nil)
	build, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		default:
			http.Redirect(w, r, internal.URL+"/admin", http.StatusFound)
		}
	})
	p := startTestProxy(t, ProxyOptions{AllowAddr: strings.TrimPrefix(build.URL, "http://")})

	// The proxy follows no redirect itself: the 302 reaches the browser.
	noFollow := viaProxy(t, p)
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noFollow.Get(build.URL + "/metadata")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status %d, want the 302 passed through unfollowed", resp.StatusCode)
	}

	// Following it, as the browser does, asks the proxy again, which refuses.
	for _, path := range []string{"/metadata", "/internal"} {
		resp, err := viaProxy(t, p).Get(build.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s followed through the proxy: status %d, want 403", path, resp.StatusCode)
		}
	}
	if internalHits.Load() != 0 {
		t.Error("a redirect reached a loopback server the proxy does not admit")
	}
}

// TestRenderedPageCannotReachPrivateAddresses drives a real Chrome: a page
// whose script fetches 127.0.0.1 and 169.254.169.254 has both refused, and
// the page still renders.
func TestRenderedPageCannotReachPrivateAddresses(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a real Chrome; runs in task test")
	}
	exe, err := FindChrome()
	if err != nil {
		t.Skipf("no Chrome on this machine: %v", err)
	}
	internal, internalHits := countingServer(t, nil)
	page := fmt.Sprintf(`<!doctype html><html lang="en"><head><title>t</title></head><body><p id="s">loading</p><script>
Promise.allSettled([fetch(%q, {mode: "no-cors"}), fetch("http://169.254.169.254/latest/meta-data/", {mode: "no-cors"})])
  .then(function () { document.getElementById("s").textContent = "done"; });
</script></body></html>`, internal.URL+"/secret")
	build, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(page))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := Start(ctx, Options{ExecPath: exe, Settle: 1500 * time.Millisecond, Proxy: ProxyOptions{AllowAddr: strings.TrimPrefix(build.URL, "http://")}})
	if err != nil {
		t.Skipf("Chrome would not start here: %v", err)
	}
	defer r.Close()
	html, _, err := r.Render(ctx, build.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `id="s">done<`) {
		t.Errorf("the page did not finish rendering:\n%s", html)
	}
	if internalHits.Load() != 0 {
		t.Errorf("the page's script reached a loopback server %d times", internalHits.Load())
	}
	if r.Refused() < 2 {
		t.Errorf("Refused = %d, want both private fetches refused by the proxy", r.Refused())
	}
}

// The build server's exception is for plain http:// forwarding only: a
// CONNECT tunnel to it is refused like any private address.
func TestProxyRefusesACONNECTToTheAllowedBuildServer(t *testing.T) {
	t.Parallel()
	build, hits := countingServer(t, nil)
	host := strings.TrimPrefix(build.URL, "http://")
	p := startTestProxy(t, ProxyOptions{AllowAddr: host})
	conn, err := net.Dial("tcp", strings.TrimPrefix(p.URL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("CONNECT to AllowAddr %s: status %d, want 403", host, resp.StatusCode)
	}
	if hits.Load() != 0 || p.Refused() != 1 {
		t.Errorf("hits %d, refused %d; want 0, 1", hits.Load(), p.Refused())
	}
	// Plain http:// to it still works.
	r2, err := viaProxy(t, p).Get(build.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Errorf("plain GET to AllowAddr: status %d, want 200", r2.StatusCode)
	}
}
