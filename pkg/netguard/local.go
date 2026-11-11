package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrNotLocal is returned (wrapped) when a [LocalOnly] connection would leave
// this machine.
var ErrNotLocal = errors.New("netguard: connection would leave this machine")

// LocalOnly limits a client to destinations that are on THIS machine at the
// moment of connecting: a loopback address, or an address currently assigned
// to one of its network interfaces. It is for a development issuer reached at
// the machine's own LAN address (so a phone and the API agree on one origin)
// where a bearer token or a credential is sent over plain HTTP, and "this
// machine" has to stay true at the socket rather than at the time somebody
// looked.
//
// Three things make that hold, and a check at configuration time would give
// none of them:
//
//   - The check is at DIAL time, on the address the socket is about to connect
//     to, and the interfaces are re-read each time. A laptop that changed
//     networks, so the address now belongs to another host, is refused.
//   - The transport ignores proxy variables. [http.ProxyFromEnvironment] exempts
//     only localhost and loopback literals, so with HTTP_PROXY set a request to
//     the machine's own LAN address would be sent to the proxy in the clear.
//   - The address is judged after resolution, so a name cannot smuggle a
//     remote address past a check on the name.
//
// This is a dev-loop guard, not a general allow-list: it admits addresses that
// [Policy] refuses (loopback, private ranges), and is never a substitute for
// it where a caller chose the destination.
type LocalOnly struct {
	// Interfaces lists the machine's interface addresses. nil is
	// [net.InterfaceAddrs]; tests inject their own.
	Interfaces func() ([]net.Addr, error)
}

// Allowed reports whether ip is loopback or assigned to an interface now.
func (l LocalOnly) Allowed(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	list := l.Interfaces
	if list == nil {
		list = net.InterfaceAddrs
	}
	addrs, err := list()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		switch v := a.(type) {
		case *net.IPNet:
			if v.IP.Equal(ip) {
				return true
			}
		case *net.IPAddr:
			if v.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

func (l LocalOnly) control(_ context.Context, _, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %q is not a host:port address", ErrNotLocal, address)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q is not an IP address at connect time", ErrNotLocal, host)
	}
	if !l.Allowed(net.IP(addr.Unmap().AsSlice())) {
		return fmt.Errorf("%w: %s is neither loopback nor an address of this machine's interfaces", ErrNotLocal, addr.Unmap())
	}
	return nil
}

// DialContext dials like a [net.Dialer] and refuses, at connect time, any
// remote address that is not local.
func (l LocalOnly) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, ControlContext: l.control}
	return d.DialContext(ctx, network, addr)
}

// Transport is an [http.Transport] that dials through [LocalOnly.DialContext]
// and never uses a proxy. Keep-alives are off so every request is a fresh
// connection and so a fresh check; the callers here are cached per subject.
// Each call returns a new Transport.
func (l LocalOnly) Transport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           l.DialContext,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}
