package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/netguard"
)

// LocalIssuerHTTPClient is nil for every issuer that is not plain HTTP at a
// non-loopback IPv4 literal, so the scaffolded caller can pass it unconditionally.
func TestLocalIssuerHTTPClient_OnlyForALANPlainHTTPIPv4Issuer(t *testing.T) {
	for _, issuer := range []string{
		"", "https://192.168.1.20:8080", "http://localhost:8080", "http://127.0.0.1:8080",
		"http://idp.internal:8080", "http://[fe80::1]:8080", "not a url",
	} {
		if c := LocalIssuerHTTPClient(issuer); c != nil {
			t.Errorf("LocalIssuerHTTPClient(%q) = %v, want nil (the default client)", issuer, c)
		}
	}
	if LocalIssuerHTTPClient("http://192.168.1.20:8080") == nil {
		t.Error("a plain-HTTP LAN IPv4 issuer must get a client")
	}
}

// The client is wired to netguard.LocalOnly: no Proxy (HTTP_PROXY would capture
// a LAN-address request, only loopback is exempt) and the connect-time guard.
func TestLocalIssuerHTTPClient_UsesTheLocalOnlyTransport(t *testing.T) {
	c := LocalIssuerHTTPClient("http://192.168.1.20:8080")
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want netguard.LocalOnly's *http.Transport", c.Transport)
	}
	if tr.Proxy != nil {
		t.Error("the transport must not consult proxy variables")
	}
	if c.Timeout == 0 {
		t.Error("the client must carry a timeout")
	}
	if _, err := tr.DialContext(context.Background(), "tcp", "192.0.2.1:80"); !errors.Is(err, netguard.ErrNotLocal) {
		t.Errorf("dialing a non-local address: err = %v, want netguard.ErrNotLocal", err)
	}
}

// A discovery-shaped request (oauth2.Discover is handed this client by the
// scaffolded auth) is refused at connect time when the address is not this
// machine's, so the hop that finds the JWKS URL cannot be answered by another
// host either. (pkg/oauth2 imports this package, so the request is built here.)
func TestLocalIssuerHTTPClient_GuardsOIDCDiscovery(t *testing.T) {
	const issuer = "http://192.0.2.1:8080"
	c := LocalIssuerHTTPClient(issuer)
	req, err := http.NewRequest(http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(req); !errors.Is(err, netguard.ErrNotLocal) {
		t.Fatalf("discovery request over the local-only client: err = %v, want netguard.ErrNotLocal", err)
	}
}

// With JWTConfig.HTTPClient the JWKS fetch goes out through that client.
type countingTransport struct {
	hits atomic.Int64
	base http.RoundTripper
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.hits.Add(1)
	return c.base.RoundTrip(r)
}

func TestJWKSHTTPClient_IsUsedForTheFetch(t *testing.T) {
	key := rsaKeyFor(t)
	url := newJWKSServer(t, "k1", rsaJWK(t, key, "k1"))
	rt := &countingTransport{base: http.DefaultTransport}
	v, err := NewValidator(Config{Provider: ProviderJWT, JWT: JWTConfig{
		SigningMethod: "RS256", JWKSURL: url, HTTPClient: &http.Client{Transport: rt},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	if rt.hits.Load() == 0 {
		t.Error("the JWKS fetch did not go through JWTConfig.HTTPClient")
	}
}

// The default is unchanged: no client set means http.DefaultTransport, and the
// caller's client is copied, never mutated.
func TestJWKSHTTPClient_DefaultIsUnchanged(t *testing.T) {
	var last atomic.Int64
	if got := jwksHTTPClient(nil, &last).Transport.(okTracker).base; got != http.DefaultTransport {
		t.Errorf("default base transport = %T, want http.DefaultTransport", got)
	}
	given := &http.Client{Timeout: time.Second}
	c := jwksHTTPClient(given, &last)
	if given.Transport != nil {
		t.Error("the caller's client was mutated")
	}
	if c.Timeout != time.Second || c.Transport.(okTracker).base != http.DefaultTransport {
		t.Errorf("a client without a transport keeps its settings and the default transport, got %+v", c)
	}
}

// End to end on a LAN-address issuer: the JWKS is fetched through the
// local-only transport, and once the address stops being this machine's the
// fetch is refused at connect time instead of trusting whoever holds it.
func TestJWKSLocalIssuerClient_FetchesAtTheLANAddressAndRefusesAfterANetworkChange(t *testing.T) {
	lan := lanIPv4()
	if lan == "" {
		t.Skip("no non-loopback IPv4 address")
	}
	l, err := net.Listen("tcp", lan+":0")
	if err != nil {
		t.Skipf("cannot listen on %s: %v", lan, err)
	}
	key := rsaKeyFor(t)
	doc, _ := json.Marshal(map[string]any{"keys": []any{rsaJWK(t, key, "k1")}})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	}))
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)

	issuer := srv.URL // http://<lan>:<port>
	v, err := NewValidator(Config{Provider: ProviderJWT, JWT: JWTConfig{
		SigningMethod: "RS256", JWKSURL: issuer + "/.well-known/jwks.json",
		HTTPClient: LocalIssuerHTTPClient(issuer),
	}})
	if err != nil {
		t.Fatalf("JWKS fetch through the local-only client: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })

	moved := localIssuerHTTPClient(issuer, func() ([]net.Addr, error) { return nil, nil })
	_, err = NewValidator(Config{Provider: ProviderJWT, JWT: JWTConfig{
		SigningMethod: "RS256", JWKSURL: issuer + "/.well-known/jwks.json", HTTPClient: moved,
	}})
	if !errors.Is(err, netguard.ErrNotLocal) {
		t.Fatalf("after the address left this machine: err = %v, want netguard.ErrNotLocal", err)
	}
}
