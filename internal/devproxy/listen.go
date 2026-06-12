package devproxy

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// ListenLoopback binds the dev proxy's port on BOTH loopback families
// — 127.0.0.1 and ::1 — and returns one listener per family.
//
// Why not a single `net.Listen("tcp", "localhost:port")`: Go binds
// only the FIRST address localhost resolves to (usually 127.0.0.1),
// while browsers resolve localhost per-request and may pick ::1
// (Chrome does). With a v4-only proxy and a service wildcard-bound on
// a nearby port, `http://localhost:<proxy>` raced between the proxy
// and the raw backend depending on which family the browser chose —
// the split-brain from journey fr-5b2121e48f. And why not a wildcard
// `:port` bind: that exposes the dev proxy on every interface (the
// LAN), which a local dev loop must not do.
//
// Family handling is explicit, not platform-assumed (macOS lets a
// loopback bind coexist with a wildcard bind where Linux returns
// EADDRINUSE):
//
//   - 127.0.0.1 must bind, or the whole call fails.
//   - ::1 busy (EADDRINUSE) is also a hard failure: another process
//     owning one family of "localhost" is exactly the half-port
//     split-brain this function exists to prevent.
//   - any other ::1 failure (no IPv6 loopback on the host) degrades to
//     v4-only, which is consistent: with no ::1, localhost can only
//     resolve to 127.0.0.1.
func ListenLoopback(port int) ([]net.Listener, error) {
	v4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("listen 127.0.0.1:%d: %w", port, err)
	}
	v6, err6 := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err6 != nil {
		if errors.Is(err6, syscall.EADDRINUSE) {
			_ = v4.Close()
			return nil, fmt.Errorf("listen [::1]:%d: %w — another process owns the IPv6 side of this port, so the browser's localhost would race between it and the dev proxy; stop it or pick a different --proxy-port", port, err6)
		}
		// No usable IPv6 loopback: v4-only is consistent.
		return []net.Listener{v4}, nil
	}
	return []net.Listener{v4, v6}, nil
}
