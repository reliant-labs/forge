package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

// tokenAuthenticator validates every non-key credential as the subject sub,
// with no email: the shape of a managed identity's access token.
func tokenAuthenticator(sub string) *Authenticator {
	return NewAuthenticator(func(string) (*Claims, error) {
		return &Claims{UserID: sub}, nil
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
	a.UseUserInfo(NewUserInfo(fixedEndpoint(srv.URL), srv.Client()))

	for range 3 {
		c, err := a.Authenticate(context.Background(), "good")
		if err != nil {
			t.Fatal(err)
		}
		if c.Email != "ann@example.com" {
			t.Fatalf("Email = %q, want the userinfo address", c.Email)
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
		a.UseUserInfo(NewUserInfo(fixedEndpoint(srv.URL), srv.Client()))
		c, err := a.Authenticate(context.Background(), "good")
		if err != nil {
			t.Fatal(err)
		}
		if c.Email != "" {
			t.Errorf("%s: an unverified address was filled: %q", name, c.Email)
		}
	}
}

// OIDC Core §5.3.2: a userinfo response for another subject is discarded.
func TestUserInfo_RefusesAResponseForAnotherSubject(t *testing.T) {
	srv, _ := userInfoServer(t, map[string]any{"sub": "someone-else", "email": "eve@example.com", "email_verified": true})
	a := tokenAuthenticator("u1")
	a.UseUserInfo(NewUserInfo(fixedEndpoint(srv.URL), srv.Client()))
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
		return &Claims{UserID: "u1", Email: "own@example.com"}, nil
	})
	a.UseUserInfo(NewUserInfo(fixedEndpoint(srv.URL), srv.Client()))
	c, err := a.Authenticate(context.Background(), "good")
	if err != nil || c.Email != "own@example.com" || calls.Load() != 0 {
		t.Fatalf("token email: Email %q err %v calls %d, want the token's own and no lookup", c.Email, err, calls.Load())
	}

	var resolves atomic.Int64
	b := tokenAuthenticator("u1")
	u := NewUserInfo(func(context.Context) (string, error) {
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
	a.UseUserInfo(NewUserInfo(fixedEndpoint(srv.URL), srv.Client()))
	if _, err := a.Authenticate(context.Background(), "fk_abcdefgh_0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("API key: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("an API key made %d userinfo calls, want 0", n)
	}
}
