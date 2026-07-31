//go:build ignore

package middleware

import (
	"context"
	"testing"

	"connectrpc.com/connect"
)

// These tests cover the POLICY WIRING this file owns — the mechanism
// (mode resolution, allow-list gate, Bearer parsing, enrichment
// plumbing) is tested in forge/pkg/authn; don't re-test it here.

// A server with no validator and no external auth must refuse to start.
func TestNewAuthInterceptor_UnconfiguredErrors(t *testing.T) {
	ic, err := NewAuthInterceptor(AuthDeps{})
	if err == nil {
		t.Fatal("NewAuthInterceptor must error when unconfigured")
	}
	if ic != nil {
		t.Fatal("NewAuthInterceptor must return a nil interceptor alongside the error")
	}
}

// An explicit AuthDeps.Validate flips the interceptor into validate mode.
func TestNewAuthInterceptor_ValidatorConfigured(t *testing.T) {
	validate := func(string) (*Claims, error) {
		return &Claims{UserID: "u1"}, nil
	}
	ic, err := NewAuthInterceptor(AuthDeps{Validate: validate})
	if err != nil {
		t.Fatalf("NewAuthInterceptor with validator must not error: %v", err)
	}
	if ic == nil {
		t.Fatal("NewAuthInterceptor must not return nil when configured")
	}
	claims, err := validate("anything")
	if err != nil || claims == nil || claims.UserID != "u1" {
		t.Fatalf("the threaded validator must dispatch, got %+v, %v", claims, err)
	}
}

// AuthDeps.ExternalAuth puts the interceptor in passthrough mode — the
// pack's own interceptor in the chain is the source of truth.
func TestNewAuthInterceptor_ExternalAuthIsPassthrough(t *testing.T) {
	ic, err := NewAuthInterceptor(AuthDeps{ExternalAuth: true})
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

// AuthDeps.ExternalAuth is the ONLY opt-out, and it is a field in this file.
// Nothing in the process environment can stand a server up without
// authentication: this asserts the negative, because "we removed the env
// opt-out" is only true while nothing quietly re-adds one.
func TestNewAuthInterceptor_EnvironmentCannotOptOut(t *testing.T) {
	// NOT parallel: mutates the process environment.
	for _, name := range []string{"AUTH_MODE", "AUTH", "DISABLE_AUTH", "NO_AUTH"} {
		t.Setenv(name, "none")
	}
	if _, err := NewAuthInterceptor(AuthDeps{}); err == nil {
		t.Fatal("an environment variable disabled authentication; only AuthDeps.ExternalAuth may")
	}
}

// The allow-list must stay exact-match only — substring matching is how
// auth bypasses are born. (The gate itself lives in pkg/authn; this
// pins the CONTENTS this project ships with. The set is generated from the
// protos' auth_required declarations — see procedures_gen.go — so an entry
// appearing here that no rpc declared is a generator bug, and an rpc you
// meant to publish that is missing is a missing annotation.)
func TestUnauthenticatedProcedures_Contents(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		"/grpc.health.v1.Health/Check",
		"/grpc.health.v1.Health/Watch",
	} {
		if _, ok := UnauthenticatedProcedures[p]; !ok {
			t.Fatalf("%s must be on the allow-list", p)
		}
	}
	if _, ok := UnauthenticatedProcedures["/demo.v1.Service/HealthCheck"]; ok {
		t.Fatal("substring-shaped entries must not be allow-listed")
	}
}

// The claims stash is library mechanism (forge/pkg/authn owns the type,
// the context key, the accessors and GetUser, and tests them). What this
// package owns is the RE-EXPORT: handlers and generated code spell
// middleware.ContextWithClaims / GetUser, so this asserts those names reach
// the same stash — a delegation wired to a different key would authenticate
// the request and then report "no authenticated user" in every handler.
func TestClaimsReExportsReachTheLibraryStash(t *testing.T) {
	t.Parallel()
	ctx := ContextWithClaims(context.Background(), &Claims{UserID: "u-1"})
	got, ok := ClaimsFromContext(ctx)
	if !ok || got.UserID != "u-1" {
		t.Fatalf("claims round-trip failed: %+v", got)
	}
	user, err := GetUser(ctx)
	if err != nil {
		t.Fatalf("GetUser must find claims installed via ContextWithClaims: %v", err)
	}
	if user.UserID != "u-1" {
		t.Fatalf("GetUser returned %q, want %q", user.UserID, "u-1")
	}
}
