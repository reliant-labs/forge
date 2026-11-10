package netguard

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func fakeInterfaces(ips ...string) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) {
		var out []net.Addr
		for _, s := range ips {
			out = append(out, &net.IPNet{IP: net.ParseIP(s), Mask: net.CIDRMask(24, 32)})
		}
		return out, nil
	}
}

func TestLocalOnly_Allowed(t *testing.T) {
	l := LocalOnly{Interfaces: fakeInterfaces("192.168.1.20")}
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "192.168.1.20": true,
		"192.168.1.21": false, "10.0.0.1": false, "8.8.8.8": false,
	} {
		if got := l.Allowed(net.ParseIP(ip)); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", ip, got, want)
		}
	}
	if l.Allowed(nil) {
		t.Error("nil is not an address of this machine")
	}
	if (LocalOnly{Interfaces: func() ([]net.Addr, error) { return nil, errors.New("no interfaces") }}).Allowed(net.ParseIP("192.168.1.20")) {
		t.Error("an unreadable interface list admits nothing but loopback")
	}
}

// The interface list is read at DIAL time: an address that stops being this
// machine's mid-process (a network change) is refused on the next connection,
// not remembered as allowed.
func TestLocalOnly_RefusesAnAddressThatLeftTheMachineAtDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	// 127.0.0.1 is always allowed, so move "the machine's address" onto a
	// non-loopback literal that dials nowhere useful, and prove the refusal
	// happens in the dial control before any connect.
	owned := true
	l := LocalOnly{Interfaces: func() ([]net.Addr, error) {
		if owned {
			return fakeInterfaces("192.0.2.77")()
		}
		return nil, nil
	}}
	tr := l.Transport()
	t.Cleanup(tr.CloseIdleConnections)
	if tr.Proxy != nil {
		t.Fatal("a local-only transport never uses a proxy")
	}
	_ = u
	if err := l.control(nil, "tcp", "192.0.2.77:80", nil); err != nil {
		t.Fatalf("owned address refused: %v", err)
	}
	owned = false
	err := l.control(nil, "tcp", "192.0.2.77:80", nil)
	if !errors.Is(err, ErrNotLocal) {
		t.Fatalf("after the address left the machine: err = %v, want ErrNotLocal", err)
	}
	// And through the real dialer: the refusal surfaces from DialContext.
	if _, err := l.DialContext(t.Context(), "tcp", "192.0.2.77:80"); !errors.Is(err, ErrNotLocal) {
		t.Fatalf("DialContext err = %v, want ErrNotLocal", err)
	}
	// A hostname is judged after resolution: this one resolves to loopback.
	c := &http.Client{Transport: l.Transport()}
	resp, err := c.Get("http://localhost:" + u.Port())
	if err != nil {
		t.Fatalf("loopback through a name: %v", err)
	}
	_ = resp.Body.Close()
}

// lanIPv4 is a non-loopback IPv4 address of this machine, "" when it has none.
func lanIPv4() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	return ""
}

// HTTP_PROXY must not capture a request to this machine's own LAN address.
// http.ProxyFromEnvironment exempts only localhost and loopback literals, so a
// request to the LAN address is the one that would go to the proxy in the
// clear: the target is bound there, not on loopback, to prove the difference.
func TestLocalOnly_IgnoresProxyVariables(t *testing.T) {
	lan := lanIPv4()
	if lan == "" {
		t.Skip("no non-loopback IPv4 address on this machine")
	}
	var proxied atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { proxied.Store(true) }))
	t.Cleanup(proxy.Close)
	l, err := net.Listen("tcp", lan+":0")
	if err != nil {
		t.Skipf("cannot listen on %s: %v", lan, err)
	}
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "direct") }))
	target.Listener = l
	target.Start()
	t.Cleanup(target.Close)

	proxyURL, _ := url.Parse(proxy.URL)
	ambient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	if resp, err := ambient.Get(target.URL); err == nil {
		_ = resp.Body.Close()
	}
	if !proxied.Load() {
		t.Fatal("fixture: a proxying transport did not use the proxy, so this test proves nothing")
	}
	proxied.Store(false)

	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	c := &http.Client{Transport: LocalOnly{}.Transport()}
	resp, err := c.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "direct" || proxied.Load() {
		t.Fatalf("body %q, proxied %v: the request went through the proxy", b, proxied.Load())
	}
}
