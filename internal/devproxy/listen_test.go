package devproxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// freeLoopbackPort grabs a port the kernel considers free on BOTH
// loopback families right now. Inherently racy (we close before the
// test rebinds), but the standard pattern for listener tests.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func hasIPv6Loopback() bool {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// ListenLoopback must bind the proxy port on BOTH loopback families.
// Journey fr-5b2121e48f: the proxy listened on 127.0.0.1 only while
// the first service wildcard-bound the same number — Chrome resolved
// localhost to ::1 and every advertised URL hit the raw API instead of
// the proxy. A v4-only bind is acceptable ONLY when the host has no
// IPv6 loopback at all; a busy ::1 side must be a hard error (another
// process owning one family is exactly the split-brain).
func TestListenLoopback_BindsBothFamilies(t *testing.T) {
	t.Parallel()
	port := freeLoopbackPort(t)

	lns, err := ListenLoopback(port)
	if err != nil {
		t.Fatalf("ListenLoopback(%d): %v", port, err)
	}
	defer func() {
		for _, ln := range lns {
			_ = ln.Close()
		}
	}()

	var addrs []string
	for _, ln := range lns {
		addrs = append(addrs, ln.Addr().String())
	}
	joined := strings.Join(addrs, " ")
	if !strings.Contains(joined, "127.0.0.1") {
		t.Fatalf("ListenLoopback must always cover 127.0.0.1, got %v", addrs)
	}
	if hasIPv6Loopback() {
		if !strings.Contains(joined, "::1") {
			t.Fatalf("host has IPv6 loopback but ListenLoopback did not bind ::1: %v", addrs)
		}
		// Both families must actually accept connections.
		for _, target := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)} {
			conn, derr := net.Dial("tcp", target)
			if derr != nil {
				t.Fatalf("dial %s: %v", target, derr)
			}
			_ = conn.Close()
		}
	}
}

// The full split-brain regression: a Router served on ListenLoopback
// listeners must answer the SAME content on http://127.0.0.1:<port>
// and http://[::1]:<port>. In journey fr-5b2121e48f the v6 side of
// the proxy port belonged to a raw backend, so the two spellings of
// localhost returned different applications.
func TestListenLoopback_RouterServesBothFamilies(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "frontend-ok")
	}))
	defer backend.Close()
	backendPort := backend.Listener.Addr().(*net.TCPAddr).Port

	router := New([]Backend{{Host: "web.localhost", Port: backendPort, Kind: "frontend", Name: "web"}})
	srv := &http.Server{Handler: router, ReadHeaderTimeout: time.Second}

	proxyPort := freeLoopbackPort(t)
	lns, err := ListenLoopback(proxyPort)
	if err != nil {
		t.Fatalf("ListenLoopback(%d): %v", proxyPort, err)
	}
	for _, ln := range lns {
		go func(ln net.Listener) { _ = srv.Serve(ln) }(ln)
	}
	defer srv.Close()

	targets := []string{fmt.Sprintf("http://127.0.0.1:%d/", proxyPort)}
	if len(lns) > 1 {
		targets = append(targets, fmt.Sprintf("http://[::1]:%d/", proxyPort))
	}
	for _, target := range targets {
		resp, gerr := http.Get(target)
		if gerr != nil {
			t.Fatalf("GET %s: %v", target, gerr)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "frontend-ok" {
			t.Fatalf("GET %s = %d %q, want 200 frontend-ok — both loopback families must reach the same app", target, resp.StatusCode, body)
		}
	}
}

func TestListenLoopback_V4Busy(t *testing.T) {
	t.Parallel()
	port := freeLoopbackPort(t)
	hold, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	defer hold.Close()

	if lns, err := ListenLoopback(port); err == nil {
		for _, ln := range lns {
			_ = ln.Close()
		}
		t.Fatalf("ListenLoopback(%d) with busy 127.0.0.1 side must error", port)
	}
}

func TestListenLoopback_V6BusyIsSplitBrainError(t *testing.T) {
	t.Parallel()
	if !hasIPv6Loopback() {
		t.Skip("no IPv6 loopback on this host")
	}
	port := freeLoopbackPort(t)
	hold, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		t.Fatalf("hold ::1: %v", err)
	}
	defer hold.Close()

	lns, err := ListenLoopback(port)
	if err == nil {
		for _, ln := range lns {
			_ = ln.Close()
		}
		t.Fatalf("ListenLoopback(%d) with busy [::1] side must error — a half-owned port is the localhost split-brain", port)
	}
	if !strings.Contains(err.Error(), "::1") {
		t.Fatalf("error must name the busy ::1 side: %v", err)
	}
}
