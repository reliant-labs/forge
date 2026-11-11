package devidp

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/reliant-labs/forge/pkg/netguard"
)

func TestPlainHTTPNonLoopback(t *testing.T) {
	for base, want := range map[string]bool{
		"http://192.168.1.20:8080": true,
		"http://idp:8080":          true,
		"http://203.0.113.9:8080":  true,
		"http://localhost:8080":    false,
		"http://LOCALHOST:8080":    false,
		"http://127.0.0.1:8080":    false,
		"http://[::1]:8080":        false,
		"https://192.168.1.20":     false,
		"":                         false,
		"://bad":                   false,
	} {
		if got := plainHTTPNonLoopback(base); got != want {
			t.Errorf("plainHTTPNonLoopback(%q) = %v, want %v", base, got, want)
		}
	}
}

// The transport is chosen by the caller's explicit LocalOnly signal and the
// shape of Base, never by looking at this machine's interfaces. (That
// LocalOnly's transport really bypasses a proxy and refuses at connect time is
// proven in pkg/netguard; here the assertion is that the client is wired to it.)
func TestClient_BaseTransportFollowsTheLocalOnlySignal(t *testing.T) {
	const lan = "http://192.168.1.20:8080"

	if got := (&Client{Base: lan}).baseTransport(); got != nil {
		t.Errorf("without LocalOnly the shared default transport is used, got %T", got)
	}
	for _, base := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "https://192.168.1.20:8080"} {
		if got := (&Client{Base: base, LocalOnly: true}).baseTransport(); got != nil {
			t.Errorf("%s: loopback and HTTPS keep the default transport even with LocalOnly, got %T", base, got)
		}
	}

	// The signal alone decides: an address that is NOT on any interface of this
	// machine still gets the guarded transport, because the decision is not
	// made from the interfaces (a network change must not flip it).
	for _, base := range []string{lan, "http://203.0.113.9:8080"} {
		rt := (&Client{Base: base, LocalOnly: true}).baseTransport()
		tr, ok := rt.(*http.Transport)
		if !ok {
			t.Fatalf("%s: LocalOnly gave %T, want an *http.Transport", base, rt)
		}
		if tr.Proxy != nil {
			t.Errorf("%s: the LocalOnly transport must not consult proxy variables", base)
		}
		if tr.DialContext == nil {
			t.Fatalf("%s: no guarded dialer", base)
		}
		// It IS netguard's guard: a non-local address is refused at dial.
		if _, err := tr.DialContext(context.Background(), "tcp", "192.0.2.1:80"); !errors.Is(err, netguard.ErrNotLocal) {
			t.Errorf("%s: dialing a non-local address: err = %v, want netguard.ErrNotLocal", base, err)
		}
	}
}
