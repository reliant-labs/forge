package pgtest

import "net"

// reserveLoopbackPort binds an ephemeral loopback TCP port, reads the
// assigned number, and releases it. embedded-postgres needs a concrete
// port; this minimizes (but cannot fully eliminate) the race between
// release and postgres binding it.
func reserveLoopbackPort() (uint32, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return uint32(l.Addr().(*net.TCPAddr).Port), nil
}
