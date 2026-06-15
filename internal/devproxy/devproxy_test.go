package devproxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newBackend spins up an httptest.NewServer that echoes a marker so
// the test can assert which backend handled the request. Returns the
// numeric loopback port.
func newBackend(t *testing.T, marker string) (string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "marker=%s host=%s path=%s", marker, r.Host, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", srv.URL, err)
	}
	_, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split %q: %v", u.Host, err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return marker, port
}

// TestRouter_DispatchByHost is the table-driven dispatch matrix —
// each row asserts a request host lands on the expected backend.
func TestRouter_DispatchByHost(t *testing.T) {
	_, adminPort := newBackend(t, "admin")
	_, webPort := newBackend(t, "web")
	_, apiPort := newBackend(t, "api")

	r := New([]Backend{
		{Host: "admin.localhost", Port: adminPort, Kind: "frontend", Name: "admin"},
		{Host: "web.localhost", Port: webPort, Kind: "frontend", Name: "web"},
		{Host: "api.localhost", Port: apiPort, Kind: "service", Name: "api"},
	})

	proxy := httptest.NewServer(r)
	defer proxy.Close()

	tests := []struct {
		name       string
		host       string
		path       string
		wantMarker string
	}{
		{"admin host matches", "admin.localhost:8080", "/", "admin"},
		{"web host matches", "web.localhost:8080", "/x", "web"},
		{"api host matches", "api.localhost:8080", "/v1", "api"},
		{"host without port matches", "admin.localhost", "/", "admin"},
		{"unknown host falls back to first frontend (admin)", "unknown.localhost:8080", "/", "admin"},
		{"bare localhost falls back to first frontend (admin)", "localhost:8080", "/", "admin"},
		{"path is preserved", "web.localhost:8080", "/_next/static/chunks/main.js", "web"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, proxy.URL+tt.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Host = tt.host
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), "marker="+tt.wantMarker) {
				t.Errorf("body=%q does not contain marker=%s", body, tt.wantMarker)
			}
		})
	}
}

// TestRouter_404OnNoDefault asserts the missing-host fallback when
// there are no frontends — services-only configurations don't get a
// silent default route; the user sees 404 so the host typo is
// obvious.
func TestRouter_404OnNoDefault(t *testing.T) {
	_, apiPort := newBackend(t, "api")
	r := New([]Backend{
		{Host: "api.localhost", Port: apiPort, Kind: "service", Name: "api"},
	})
	proxy := httptest.NewServer(r)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
	req.Host = "nope.localhost:8080"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestRouter_502OnBackendDown asserts the error path when a route is
// declared but the backend isn't listening. This is a common dev
// case (`forge run` started, then a single frontend's npm run crashed)
// and the 502 surface keeps the failure isolated to the affected host
// instead of taking down the whole proxy.
func TestRouter_502OnBackendDown(t *testing.T) {
	// Bind+immediately-close to grab an unused port that nothing is on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	r := New([]Backend{
		{Host: "dead.localhost", Port: port, Kind: "frontend", Name: "dead"},
	})
	proxy := httptest.NewServer(r)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
	req.Host = "dead.localhost:8080"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

// TestRouter_WebSocketSplice asserts the WS upgrade path forwards
// frames bidirectionally — the load-bearing assertion for Next.js
// HMR. We stand up a tiny WS-shaped backend that hijacks its own
// connection, echoes a known token after the 101 handshake, then
// dial through the proxy and assert both halves round-trip.
//
// We can't pull in a real WS implementation just for the test, so the
// backend uses the raw "HTTP/1.1 101 Switching Protocols" + io.Copy
// shape — same shape the proxy splices, exercised against itself.
func TestRouter_WebSocketSplice(t *testing.T) {
	// Backend: accept the WS upgrade, send 101 + a known token, then
	// echo whatever the client writes.
	backendL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = backendL.Close() })
	backendPort := backendL.Addr().(*net.TCPAddr).Port

	backendDone := make(chan struct{})
	go func() {
		defer close(backendDone)
		conn, err := backendL.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read the request line + headers; we don't actually parse,
		// just drain until \r\n\r\n.
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		// 101 + server token.
		_, _ = conn.Write([]byte(
			"HTTP/1.1 101 Switching Protocols\r\n" +
				"Upgrade: websocket\r\n" +
				"Connection: Upgrade\r\n" +
				"\r\n" +
				"SERVER-HELLO\n",
		))
		// Echo client bytes back.
		buf := make([]byte, 64)
		n, err := br.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write(append([]byte("ECHO:"), buf[:n]...))
	}()

	r := New([]Backend{
		{Host: "ws.localhost", Port: backendPort, Kind: "frontend", Name: "ws"},
	})
	proxy := httptest.NewServer(r)
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)

	// Client: open a raw TCP connection to the proxy, send a WS-shaped
	// upgrade request, assert the 101 + server token reach us, write a
	// client token, and assert the echo comes back.
	client, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("Dial proxy: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := client.Write([]byte(
		"GET /chat HTTP/1.1\r\n" +
			"Host: ws.localhost:8080\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"\r\n",
	)); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}

	br := bufio.NewReader(client)
	// Read the status line.
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("status = %q, want 101", statusLine)
	}
	// Drain headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	// Server hello.
	hello, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if !strings.Contains(hello, "SERVER-HELLO") {
		t.Errorf("hello = %q, want SERVER-HELLO", hello)
	}
	// Client → server.
	if _, err := client.Write([]byte("PING\n")); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	// Server echoes.
	buf := make([]byte, 64)
	n, err := br.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read echo: %v", err)
	}
	got := string(buf[:n])
	if !strings.Contains(got, "ECHO:PING") {
		t.Errorf("echo = %q, want to contain ECHO:PING", got)
	}

	<-backendDone
}

// TestNormalizeHost covers the host-stripping helper directly so the
// dispatch-by-host tests don't need to cover every shape.
func TestNormalizeHost(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"admin.localhost:8080", "admin.localhost"},
		{"admin.localhost", "admin.localhost"},
		{"Admin.LocalHost:8080", "admin.localhost"},
		{"", ""},
		{"127.0.0.1:9090", "127.0.0.1"},
	}
	for _, tt := range tests {
		if got := normalizeHost(tt.in); got != tt.want {
			t.Errorf("normalizeHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestIsWebSocketUpgrade covers Connection-header tokenisation —
// browsers send `Connection: keep-alive, Upgrade` and we must match
// the Upgrade token without being fooled by `keep-alive`.
func TestIsWebSocketUpgrade(t *testing.T) {
	tests := []struct {
		name       string
		upgrade    string
		connection string
		want       bool
	}{
		{"canonical", "websocket", "Upgrade", true},
		{"lowercase", "websocket", "upgrade", true},
		{"with keep-alive", "websocket", "keep-alive, Upgrade", true},
		{"no upgrade header", "", "Upgrade", false},
		{"upgrade not websocket", "h2c", "Upgrade", false},
		{"no connection upgrade", "websocket", "keep-alive", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &http.Request{Header: http.Header{}}
			if tt.upgrade != "" {
				req.Header.Set("Upgrade", tt.upgrade)
			}
			if tt.connection != "" {
				req.Header.Set("Connection", tt.connection)
			}
			if got := isWebSocketUpgrade(req); got != tt.want {
				t.Errorf("isWebSocketUpgrade = %v, want %v", got, tt.want)
			}
		})
	}
}
