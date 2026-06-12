package devproxy

import (
	"fmt"
	"net"
	"strings"
	"testing"
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
