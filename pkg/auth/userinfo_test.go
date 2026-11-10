package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// userInfoServer answers the userinfo endpoint with body for the bearer token
// "good", and counts the requests.
func userInfoServer(t *testing.T, body map[string]any) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

const testIssuer = "https://issuer-a.example.test"

// tokenAuthenticator validates every non-key credential as the subject sub
// of testIssuer, with no email: the shape of a managed identity's access token.
func tokenAuthenticator(sub string) *Authenticator {
	return issuerAuthenticator(testIssuer, sub)
}

// issuerAuthenticator validates every non-key credential as subject sub of iss.
func issuerAuthenticator(iss, sub string) *Authenticator {
	return NewAuthenticator(func(string) (*Claims, error) {
		return &Claims{UserID: sub, Raw: map[string]any{"iss": iss, "sub": sub}}, nil
	})
}

func fixedEndpoint(url string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return url, nil }
}

// TestUserInfo_FillsAVerifiedEmailOncePerSubject is the reported gap: the
// access token names only a subject, and the enrich seam needs the address.
func TestUserInfo_FillsAVerifiedEmailOncePerSubject(t *testing.T) {
	srv, calls := userInfoServer(t, map[string]any{"sub": "u1", "email": "ann@example.com", "email_verified": true})
	a := tokenAuthenticator("u1")
	a.UseUserInfo(NewUserInfo(testIssuer, fixedEndpoint(srv.URL), srv.Client()))

	for range 3 {
		c, err := a.Authenticate(context.Background(), "good")
		if err != nil {
			t.Fatal(err)
		}
		if c.Email != "ann@example.com" || !c.EmailVerified {
			t.Fatalf("Email = %q (verified %v), want the userinfo address, verified", c.Email, c.EmailVerified)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("three requests by one subject made %d userinfo calls, want 1 (cached)", n)
	}
}

// An address the identity provider has not verified is not filled: binding an
// invitation to it would give the invitation to whoever typed it first.
func TestUserInfo_IgnoresAnUnverifiedEmail(t *testing.T) {
	for name, v := range map[string]any{"false": false, "absent": nil, "string false": "false"} {
		body := map[string]any{"sub": "u1", "email": "ann@example.com"}
		if v != nil {
			body["email_verified"] = v
		}
		srv, _ := userInfoServer(t, body)
		a := tokenAuthenticator("u1")
		a.UseUserInfo(NewUserInfo(testIssuer, fixedEndpoint(srv.URL), srv.Client()))
		c, err := a.Authenticate(context.Background(), "good")
		if err != nil {
			t.Fatal(err)
		}
		if c.Email != "" || c.EmailVerified {
			t.Errorf("%s: an unverified address was filled: %q (verified %v)", name, c.Email, c.EmailVerified)
		}
	}
}

// OIDC Core §5.3.2: a userinfo response for another subject is discarded.
func TestUserInfo_RefusesAResponseForAnotherSubject(t *testing.T) {
	srv, _ := userInfoServer(t, map[string]any{"sub": "someone-else", "email": "eve@example.com", "email_verified": true})
	a := tokenAuthenticator("u1")
	a.UseUserInfo(NewUserInfo(testIssuer, fixedEndpoint(srv.URL), srv.Client()))
	c, err := a.Authenticate(context.Background(), "good")
	if err != nil {
		t.Fatal(err)
	}
	if c.Email != "" {
		t.Errorf("an answer about another subject filled Email = %q", c.Email)
	}
}

// A token that carries its own email is not looked up, and a failing lookup or
// endpoint never fails the request: the token was valid.
func TestUserInfo_NeverOverridesNorFailsAuthentication(t *testing.T) {
	srv, calls := userInfoServer(t, map[string]any{"sub": "u1", "email": "other@example.com", "email_verified": true})
	a := NewAuthenticator(func(string) (*Claims, error) {
		return &Claims{UserID: "u1", Email: "own@example.com", Raw: map[string]any{"iss": testIssuer}}, nil
	})
	a.UseUserInfo(NewUserInfo(testIssuer, fixedEndpoint(srv.URL), srv.Client()))
	c, err := a.Authenticate(context.Background(), "good")
	if err != nil || c.Email != "own@example.com" || calls.Load() != 0 {
		t.Fatalf("token email: Email %q err %v calls %d, want the token's own and no lookup", c.Email, err, calls.Load())
	}

	var resolves atomic.Int64
	b := tokenAuthenticator("u1")
	u := NewUserInfo(testIssuer, func(context.Context) (string, error) {
		resolves.Add(1)
		return "", errors.New("issuer unreachable")
	}, nil)
	clock := time.Unix(1000, 0)
	u.now = func() time.Time { return clock }
	b.UseUserInfo(u)
	for range 3 {
		c, err := b.Authenticate(context.Background(), "good")
		if err != nil {
			t.Fatalf("an unreachable userinfo endpoint failed authentication: %v", err)
		}
		if c.Email != "" {
			t.Fatalf("Email = %q with no endpoint", c.Email)
		}
	}
	if n := resolves.Load(); n != 1 {
		t.Errorf("the endpoint was resolved %d times inside the retry window, want 1", n)
	}
	clock = clock.Add(userInfoRetry + time.Second)
	if _, err := b.Authenticate(context.Background(), "good"); err != nil {
		t.Fatal(err)
	}
	if n := resolves.Load(); n != 2 {
		t.Errorf("after the retry window the endpoint was resolved %d times in total, want 2", n)
	}
}

// API keys are the application's own principals; nothing is looked up for them.
func TestUserInfo_NotConsultedForAPIKeys(t *testing.T) {
	srv, calls := userInfoServer(t, map[string]any{"sub": "u1", "email": "ann@example.com", "email_verified": true})
	a := tokenAuthenticator("u1")
	a.UseKeyValidator(KeyValidatorFunc(func(context.Context, string) (*Claims, error) {
		return &Claims{UserID: "key-owner"}, nil
	}))
	a.UseUserInfo(NewUserInfo(testIssuer, fixedEndpoint(srv.URL), srv.Client()))
	if _, err := a.Authenticate(context.Background(), "fk_abcdefgh_0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("API key: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("an API key made %d userinfo calls, want 0", n)
	}
}

// A subject is unique only within its issuer: a token from another issuer
// with the same subject is neither looked up nor served the cached answer.
func TestUserInfo_AnotherIssuersSameSubjectGetsNothing(t *testing.T) {
	srv, calls := userInfoServer(t, map[string]any{"sub": "123", "email": "ann@example.com", "email_verified": true})
	u := NewUserInfo(testIssuer, fixedEndpoint(srv.URL), srv.Client())

	a := issuerAuthenticator(testIssuer, "123")
	a.UseUserInfo(u)
	c, err := a.Authenticate(context.Background(), "good")
	if err != nil || c.Email != "ann@example.com" {
		t.Fatalf("issuer A: Email %q err %v, want the userinfo address", c.Email, err)
	}

	for name, iss := range map[string]string{"other issuer": "https://issuer-b.example.test", "no iss": ""} {
		b := issuerAuthenticator(iss, "123")
		if iss == "" {
			b = NewAuthenticator(func(string) (*Claims, error) { return &Claims{UserID: "123"}, nil })
		}
		b.UseUserInfo(u)
		c, err := b.Authenticate(context.Background(), "good")
		if err != nil {
			t.Fatal(err)
		}
		if c.Email != "" {
			t.Errorf("%s: subject 123 received issuer A's address %q", name, c.Email)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("userinfo calls = %d, want 1 (issuer A only)", n)
	}
}

// recordingTransport records every request it is asked to send.
type recordingTransport struct {
	next http.RoundTripper
	urls []string
	auth []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.urls = append(r.urls, req.URL.String())
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	if r.next == nil {
		return nil, errors.New("recordingTransport: no network")
	}
	return r.next.RoundTrip(req)
}

// The bearer token never travels to a plain-HTTP, non-loopback userinfo URL.
func TestUserInfo_RefusesAnInsecureEndpoint(t *testing.T) {
	for _, endpoint := range []string{"http://issuer.example.test/userinfo", "http://10.0.0.1/userinfo", "ftp://issuer.example.test/userinfo"} {
		rt := &recordingTransport{}
		a := tokenAuthenticator("u1")
		a.UseUserInfo(NewUserInfo(testIssuer, fixedEndpoint(endpoint), &http.Client{Transport: rt}))
		c, err := a.Authenticate(context.Background(), "good")
		if err != nil {
			t.Fatal(err)
		}
		if c.Email != "" || len(rt.urls) != 0 {
			t.Errorf("%s: Email %q, requests %v; want no request sent", endpoint, c.Email, rt.urls)
		}
	}
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

// lanUserInfoServer serves userinfo for the bearer "good" at this machine's LAN
// address (not loopback), so a request reaches it only if it was dialled there.
func lanUserInfoServer(t *testing.T, lan string, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	l, err := net.Listen("tcp", lan+":0")
	if err != nil {
		t.Skipf("cannot listen on %s: %v", lan, err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": "u1", "email": "a@b.c", "email_verified": true})
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func lanAuthenticator(issuer, endpoint string, mutate func(*UserInfo)) (*Authenticator, *UserInfo) {
	a := issuerAuthenticator(issuer, "u1")
	ui := NewUserInfo(issuer, fixedEndpoint(endpoint), nil)
	if mutate != nil {
		mutate(ui)
	}
	a.UseUserInfo(ui)
	return a, ui
}

// DEFAULT OFF. Without AllowLocalPlainHTTP, an issuer at this machine's own LAN
// address is just a non-loopback plain-HTTP host: no token is sent. This is
// production's behaviour and must not change.
func TestUserInfo_LANAddressIsRefusedByDefault(t *testing.T) {
	lan := lanIPv4()
	if lan == "" {
		t.Skip("no non-loopback IPv4 address")
	}
	var hits atomic.Int64
	srv := lanUserInfoServer(t, lan, &hits)
	a, _ := lanAuthenticator(srv.URL, srv.URL+"/userinfo", nil)
	c, err := a.Authenticate(context.Background(), "good")
	if err != nil {
		t.Fatal(err)
	}
	if c.Email != "" || hits.Load() != 0 {
		t.Errorf("Email %q, %d requests: a LAN plain-HTTP issuer must not be reachable unless opted in", c.Email, hits.Load())
	}
}

// With the opt-in the lookup works against the LAN address, and a proxy in the
// environment is not used: ProxyFromEnvironment would send this request to the
// proxy because only loopback is exempt.
func TestUserInfo_AllowLocalPlainHTTP_WorksAndBypassesAProxy(t *testing.T) {
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

	var hits atomic.Int64
	srv := lanUserInfoServer(t, lan, &hits)
	a, _ := lanAuthenticator(srv.URL, srv.URL+"/userinfo", func(u *UserInfo) { u.AllowLocalPlainHTTP() })
	c, err := a.Authenticate(context.Background(), "good")
	if err != nil {
		t.Fatal(err)
	}
	if c.Email != "a@b.c" || !c.EmailVerified {
		t.Errorf("Email %q verified %v, want the verified address", c.Email, c.EmailVerified)
	}
	if proxied.Load() {
		t.Error("the token went to the proxy in the clear")
	}
	if hits.Load() != 1 {
		t.Errorf("%d requests reached the LAN listener, want 1", hits.Load())
	}
}

// The "this machine" decision is made at DIAL time against the interfaces as
// they are then, not once at resolve time: after the address leaves the
// machine's interfaces the cached endpoint is still refused at connect.
func TestUserInfo_AllowLocalPlainHTTP_RefusedAtDialWhenTheAddressLeaves(t *testing.T) {
	lan := lanIPv4()
	if lan == "" {
		t.Skip("no non-loopback IPv4 address")
	}
	var hits atomic.Int64
	srv := lanUserInfoServer(t, lan, &hits)
	owned := true
	a, ui := lanAuthenticator(srv.URL, srv.URL+"/userinfo", func(u *UserInfo) {
		u.interfaces = func() ([]net.Addr, error) {
			if !owned {
				return nil, nil // the network changed: nothing is ours any more
			}
			return []net.Addr{&net.IPNet{IP: net.ParseIP(lan), Mask: net.CIDRMask(24, 32)}}, nil
		}
		u.AllowLocalPlainHTTP()
	})
	c, err := a.Authenticate(context.Background(), "good")
	if err != nil || c.Email != "a@b.c" {
		t.Fatalf("while the address is ours: Email %q err %v", c.Email, err)
	}
	if ui.resolved == "" {
		t.Fatal("fixture: the endpoint should be cached after the first lookup")
	}
	owned = false
	ui.mu.Lock()
	ui.cache = map[string]userInfoEntry{} // force a new request on the cached endpoint
	ui.mu.Unlock()
	before := hits.Load()
	c, err = a.Authenticate(context.Background(), "good")
	if err != nil {
		t.Fatal(err)
	}
	if c.Email != "" || hits.Load() != before {
		t.Errorf("Email %q, %d new requests: after the address left this machine the connection must be refused at dial", c.Email, hits.Load()-before)
	}
}

// The userinfo URL must have the issuer's own origin: a discovery document
// naming another host (even another local port or address) is not followed.
func TestUserInfo_AllowLocalPlainHTTP_RefusesACrossOriginUserinfo(t *testing.T) {
	lan := lanIPv4()
	if lan == "" {
		t.Skip("no non-loopback IPv4 address")
	}
	var hits, otherHits atomic.Int64
	issuer := lanUserInfoServer(t, lan, &hits)
	other := lanUserInfoServer(t, lan, &otherHits) // same address, different port: another origin
	for _, endpoint := range []string{other.URL + "/userinfo", "http://192.0.2.9:80/userinfo", "http://example.test/userinfo"} {
		a, _ := lanAuthenticator(issuer.URL, endpoint, func(u *UserInfo) { u.AllowLocalPlainHTTP() })
		c, err := a.Authenticate(context.Background(), "good")
		if err != nil {
			t.Fatal(err)
		}
		if c.Email != "" {
			t.Errorf("%s: Email %q, a cross-origin userinfo must be refused", endpoint, c.Email)
		}
	}
	if hits.Load() != 0 || otherHits.Load() != 0 {
		t.Errorf("requests: issuer %d other %d, want none sent", hits.Load(), otherHits.Load())
	}
}

// The opt-in changes nothing for any issuer it is not meant for: HTTPS, a
// loopback dev issuer and a name keep exactly the default rules.
func TestUserInfo_AllowLocalPlainHTTP_IsInertForOtherIssuers(t *testing.T) {
	for _, issuer := range []string{"https://issuer.example.test", "http://localhost:8080", "http://127.0.0.1:8080", "http://my-mac.local:8080", "http://[fd00::1]:8080"} {
		ui := NewUserInfo(issuer, fixedEndpoint(""), nil).AllowLocalPlainHTTP()
		if ui.localHTTP {
			t.Errorf("%s: the opt-in applied to an issuer it is not for", issuer)
		}
	}
	for _, endpoint := range []string{"http://192.168.1.20:8080/userinfo", "http://my-mac.local/userinfo"} {
		u, _ := url.Parse(endpoint)
		if secureEndpoint(u) == nil {
			t.Errorf("secureEndpoint(%s) allowed: the default must refuse every non-loopback plain-HTTP host", endpoint)
		}
	}
}

// A redirect from HTTPS to plain HTTP is refused, even with a supplied client.
func TestUserInfo_RefusesARedirectDowngrade(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://issuer.example.test/userinfo", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	rt := &recordingTransport{next: srv.Client().Transport}
	a := tokenAuthenticator("u1")
	a.UseUserInfo(NewUserInfo(testIssuer, fixedEndpoint(srv.URL), &http.Client{Transport: rt}))
	c, err := a.Authenticate(context.Background(), "good")
	if err != nil {
		t.Fatal(err)
	}
	if c.Email != "" {
		t.Errorf("Email = %q after a refused redirect", c.Email)
	}
	if len(rt.urls) != 1 || rt.urls[0] != srv.URL {
		t.Errorf("requests = %v, want only the HTTPS endpoint (the redirect to HTTP refused)", rt.urls)
	}
}

// Claims.EmailVerified from a token's own claims: only an explicit
// email_verified (bool, or the string some issuers send) alongside an email.
func TestDefaultProjectClaims_EmailVerified(t *testing.T) {
	for name, tc := range map[string]struct {
		claims map[string]any
		want   bool
	}{
		"verified":        {map[string]any{"sub": "u", "email": "a@b.c", "email_verified": true}, true},
		"string verified": {map[string]any{"sub": "u", "email": "a@b.c", "email_verified": "true"}, true},
		"unverified":      {map[string]any{"sub": "u", "email": "a@b.c", "email_verified": false}, false},
		"absent":          {map[string]any{"sub": "u", "email": "a@b.c"}, false},
		"no email":        {map[string]any{"sub": "u", "email_verified": true}, false},
	} {
		if got := defaultProjectClaims(tc.claims, claimOpts{}).EmailVerified; got != tc.want {
			t.Errorf("%s: EmailVerified = %v, want %v", name, got, tc.want)
		}
	}
}
