//go:build ignore

package middleware

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
)

// These tests cover the POLICY WIRING this file owns — the mechanism
// (mode resolution, allow-list gate, Bearer parsing, enrichment
// plumbing) is tested in forge/pkg/authn; don't re-test it here.

// setValidateToken swaps the validator for one subtest and restores it
// via t.Cleanup, going through the public SetTokenValidator API.
func setValidateToken(t *testing.T, fn func(string) (*Claims, error)) {
	t.Helper()
	authMu.Lock()
	orig := validateTokenFn
	authMu.Unlock()
	SetTokenValidator(fn)
	t.Cleanup(func() {
		authMu.Lock()
		validateTokenFn = orig
		authMu.Unlock()
	})
}

// withExternalAuth marks external auth registered for one subtest.
func withExternalAuth(t *testing.T) {
	t.Helper()
	authMu.Lock()
	orig := externalAuthRegistered
	externalAuthRegistered = true
	authMu.Unlock()
	t.Cleanup(func() {
		authMu.Lock()
		externalAuthRegistered = orig
		authMu.Unlock()
	})
}

// A server with no validator, no external auth, no AUTH_MODE=none, and
// devMode false must refuse to start.
func TestNewAuthInterceptor_UnconfiguredErrors(t *testing.T) {
	// NOT parallel: reads package-level state other subtests mutate.
	t.Setenv("AUTH_MODE", "")
	ic, err := NewAuthInterceptor(false)
	if err == nil {
		t.Fatal("NewAuthInterceptor must error when unconfigured outside dev mode")
	}
	if ic != nil {
		t.Fatal("NewAuthInterceptor must return a nil interceptor alongside the error")
	}
}

// SetTokenValidator flips the interceptor into validate mode; the
// installed validator is reachable through ValidateToken.
func TestNewAuthInterceptor_ValidatorConfigured(t *testing.T) {
	t.Setenv("AUTH_MODE", "")
	setValidateToken(t, func(string) (*Claims, error) {
		return &Claims{UserID: "u1"}, nil
	})
	ic, err := NewAuthInterceptor(false)
	if err != nil {
		t.Fatalf("NewAuthInterceptor with validator must not error: %v", err)
	}
	if ic == nil {
		t.Fatal("NewAuthInterceptor must not return nil when configured")
	}
	claims, err := ValidateToken("anything")
	if err != nil || claims == nil || claims.UserID != "u1" {
		t.Fatalf("ValidateToken must dispatch to the installed validator, got %+v, %v", claims, err)
	}
}

// MarkExternalAuth puts the interceptor in passthrough mode — the
// pack's own interceptor in the chain is the source of truth.
func TestNewAuthInterceptor_ExternalAuthIsPassthrough(t *testing.T) {
	t.Setenv("AUTH_MODE", "")
	withExternalAuth(t)

	ic, err := NewAuthInterceptor(false)
	if err != nil {
		t.Fatalf("NewAuthInterceptor with external auth must not error: %v", err)
	}
	called := false
	wrapped := ic.WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	})
	_, _ = wrapped(context.Background(), nil)
	if !called {
		t.Fatal("passthrough WrapUnary must invoke next unconditionally")
	}
}

// Dev mode (injected, not read from the environment) is an explicit
// opt-in to running without an auth provider.
func TestNewAuthInterceptor_DevModeIsPassthrough(t *testing.T) {
	t.Setenv("AUTH_MODE", "")
	ic, err := NewAuthInterceptor(true)
	if err != nil {
		t.Fatalf("NewAuthInterceptor in dev mode must not error: %v", err)
	}
	if ic == nil {
		t.Fatal("NewAuthInterceptor must not return nil in dev mode")
	}
}

// AUTH_MODE=none is the explicit production opt-out.
func TestNewAuthInterceptor_AuthModeNoneIsPassthrough(t *testing.T) {
	t.Setenv("AUTH_MODE", "none")
	ic, err := NewAuthInterceptor(false)
	if err != nil {
		t.Fatalf("NewAuthInterceptor with AUTH_MODE=none must not error: %v", err)
	}
	if ic == nil {
		t.Fatal("NewAuthInterceptor must not return nil with AUTH_MODE=none")
	}
}

// The allow-list must stay exact-match only — substring matching is how
// auth bypasses are born. (The gate itself lives in pkg/authn; this
// pins the CONTENTS this project ships with.)
func TestUnauthenticatedProcedures_Contents(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		"/grpc.health.v1.Health/Check",
		"/grpc.health.v1.Health/Watch",
	} {
		if _, ok := unauthenticatedProcedures[p]; !ok {
			t.Fatalf("%s must be on the allow-list", p)
		}
	}
	if _, ok := unauthenticatedProcedures["/demo.v1.Service/HealthCheck"]; ok {
		t.Fatal("substring-shaped entries must not be allow-listed")
	}
}

// Claims round-trip through this package's context key.
func TestClaimsContextRoundTrip(t *testing.T) {
	t.Parallel()
	if _, ok := ClaimsFromContext(context.Background()); ok {
		t.Fatal("background context must carry no claims")
	}
	ctx := ContextWithClaims(context.Background(), &Claims{UserID: "u-1"})
	got, ok := ClaimsFromContext(ctx)
	if !ok || got.UserID != "u-1" {
		t.Fatalf("claims round-trip failed: %+v", got)
	}
	if _, err := GetUser(ctx); err != nil {
		t.Fatalf("GetUser must find stashed claims: %v", err)
	}
	if _, err := GetUser(context.Background()); err == nil {
		t.Fatal("GetUser must error without claims")
	}
}

func TestVerifyAuth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	if err := VerifyAuth(ctx, "admin"); err == nil {
		t.Fatal("want error when no claims on context")
	} else {
		var cerr *connect.Error
		if !errors.As(err, &cerr) || cerr.Code() != connect.CodeUnauthenticated {
			t.Fatalf("want Unauthenticated, got %v", err)
		}
	}

	withClaims := ContextWithClaims(ctx, &Claims{Role: "admin", Roles: []string{"editor"}})
	if err := VerifyAuth(withClaims); err != nil {
		t.Fatalf("no-role check should pass when claims present: %v", err)
	}
	if err := VerifyAuth(withClaims, "admin"); err != nil {
		t.Fatalf("role match on Role field should pass: %v", err)
	}
	if err := VerifyAuth(withClaims, "editor"); err != nil {
		t.Fatalf("role match on Roles slice should pass: %v", err)
	}
	if err := VerifyAuth(withClaims, "owner"); err == nil {
		t.Fatal("role miss should fail")
	} else {
		var cerr *connect.Error
		if !errors.As(err, &cerr) || cerr.Code() != connect.CodePermissionDenied {
			t.Fatalf("want PermissionDenied, got %v", err)
		}
	}
}

// DevAuthorizer is allow-all; it must satisfy the Authorizer interface
// the generated wire_gen.go swaps in under dev mode.
func TestDevAuthorizer(t *testing.T) {
	t.Parallel()
	var a Authorizer = DevAuthorizer{}
	if err := a.CanAccess(context.Background(), "/x.v1.X/Do"); err != nil {
		t.Fatalf("DevAuthorizer must allow: %v", err)
	}
	if err := a.Can(context.Background(), nil, ActionDelete, "thing"); err != nil {
		t.Fatalf("DevAuthorizer must allow: %v", err)
	}
}
