package devidp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func lanIPv4() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	return ""
}

func TestLocalPlainHTTPBase(t *testing.T) {
	lan := lanIPv4()
	for base, want := range map[string]bool{
		"http://localhost:8080":    false, // loopback: the default transport already treats it as local
		"http://127.0.0.1:8080":    false,
		"https://" + lan + ":8080": false,
		"http://idp:8080":          false, // a name proves nothing
		"http://203.0.113.9:8080":  false, // not this machine's
		"http://[fd00::1]:8080":    false,
		"":                         false,
		"://bad":                   false,
	} {
		if got := localPlainHTTPBase(base); got != want {
			t.Errorf("localPlainHTTPBase(%q) = %v, want %v", base, got, want)
		}
	}
	if lan != "" && !localPlainHTTPBase("http://"+lan+":8080") {
		t.Errorf("http://%s:8080 is this machine's own LAN address and must be guarded", lan)
	}
}

// The broker's IdP client sends the service token and users' credentials to
// the IdP at the machine's LAN address in the clear. An ambient proxy must not
// see them.
func TestClient_LocalPlainHTTPBaseIgnoresProxyVariables(t *testing.T) {
	lan := lanIPv4()
	if lan == "" {
		t.Skip("no non-loopback IPv4 address")
	}
	var proxied atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { proxied.Store(true) }))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	l, err := net.Listen("tcp", lan+":0")
	if err != nil {
		t.Skipf("cannot listen on %s: %v", lan, err)
	}
	idp := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/debug/ready" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		http.NotFound(w, r)
	}))
	idp.Listener = l
	idp.Start()
	t.Cleanup(idp.Close)

	c := &Client{Base: idp.URL, Token: "service-token"}
	if err := c.WaitReady(context.Background()); err != nil {
		t.Fatalf("the client could not reach the IdP at the LAN address: %v", err)
	}
	if proxied.Load() {
		t.Fatal("the broker's IdP traffic went to the proxy in the clear")
	}
}
