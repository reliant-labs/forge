//go:build ignore

package middleware

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
)

// setValidateToken swaps the package-level token validator for the duration
// of a single subtest and restores it via t.Cleanup. This is the only safe
// way to exercise the real middleware without stubbing its call tree.
//
// It goes through [SetTokenValidator] (not raw assignment) so the configured
// flag tracks the swap — exercising the real public API path.
func setValidateToken(t *testing.T, fn func(string) (*Claims, error)) {
	t.Helper()
	authMu.Lock()
	origFn, origConfigured := validateTokenFn, validatorConfigured
	authMu.Unlock()
	SetTokenValidator(fn)
	t.Cleanup(func() {
		authMu.Lock()
		validateTokenFn = origFn
		validatorConfigured = origConfigured
		authMu.Unlock()
	})
}

// withExternalAuth marks external-auth registered for the duration of a
// single subtest. Tests that exercise [AuthInterceptor] need to either
// register an external provider, register a real validator, or set an env
// var — otherwise the constructor panics by design.
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

// The AuthInterceptor's public entrypoint forwards to authenticateFromHeader
// and isUnauthenticatedProcedure. We test those two collaborators directly
// because connect.AnyRequest is a sealed interface (internalOnly()) — only
// the generated Connect shim can construct one with an arbitrary Procedure,
// and wiring up a real Connect client/server purely to exercise an
// if/else branch would add an outsized dependency to the scaffold tests.
func TestAuthenticateFromHeader(t *testing.T) {
	t.Parallel()

	validClaims := &Claims{UserID: "user-1", Email: "u@example.com"}

	tests := []struct {
		name          string
		authorization string
		validate      func(string) (*Claims, error)
		wantErrCode   connect.Code // 0 → success
		wantClaims    bool
	}{
		{
			name: "missing token passes through unauthenticated",
		},
		{
			name:          "malformed authorization header is rejected",
			authorization: "Token abc", // not a Bearer token
			wantErrCode:   connect.CodeUnauthenticated,
		},
		{
			name:          "invalid bearer token is rejected",
			authorization: "Bearer bad",
			validate:      func(string) (*Claims, error) { return nil, errors.New("bad sig") },
			wantErrCode:   connect.CodeUnauthenticated,
		},
		{
			name:          "valid bearer token attaches claims",
			authorization: "Bearer good",
			validate:      func(string) (*Claims, error) { return validClaims, nil },
			wantClaims:    true,
		},
	}

	// Subtests mutate validateTokenFn via setValidateToken, so they cannot
	// safely run in parallel with each other. The outer test is still
	// parallelised with the rest of the package via t.Parallel above.
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.validate != nil {
				setValidateToken(t, tc.validate)
			}

			ctx, err := authenticateFromHeader(context.Background(), tc.authorization)

			if tc.wantErrCode != 0 {
				if err == nil {
					t.Fatalf("want connect error %s, got nil", tc.wantErrCode)
				}
				var cerr *connect.Error
				if !errors.As(err, &cerr) {
					t.Fatalf("want *connect.Error, got %T: %v", err, err)
				}
				if cerr.Code() != tc.wantErrCode {
					t.Fatalf("want code %s, got %s", tc.wantErrCode, cerr.Code())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got, ok := ClaimsFromContext(ctx)
			if tc.wantClaims {
				if !ok || got == nil {
					t.Fatal("expected claims on context")
				}
				if got.UserID != validClaims.UserID {
					t.Fatalf("wrong claims attached: %+v", got)
				}
			} else if ok {
				t.Fatalf("did not expect claims, got %+v", got)
			}
		})
	}
}

func TestIsUnauthenticatedProcedure(t *testing.T) {
	t.Parallel()
	if !isUnauthenticatedProcedure("/grpc.health.v1.Health/Check") {
		t.Fatal("health Check should be allow-listed")
	}
	if !isUnauthenticatedProcedure("/grpc.health.v1.Health/Watch") {
		t.Fatal("health Watch should be allow-listed")
	}
	// Substring-containing procedures must not be matched: the allow-list
	// is deliberately exact to prevent accidental bypass.
	if isUnauthenticatedProcedure("/grpc.health.v1.Health/Report") {
		t.Fatal("Health/Report is not in the allow-list")
	}
	if isUnauthenticatedProcedure("/demo.v1.Service/HealthCheck") {
		t.Fatal("user-defined HealthCheck must not be matched by substring")
	}
	if isUnauthenticatedProcedure("") {
		t.Fatal("empty procedure must not match")
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

// Default ValidateToken is a passthrough (nil claims, nil error). The
// "no auth configured" case is rejected at AuthInterceptor construction
// time, not per request — see TestAuthInterceptor_UnconfiguredPanics.
func TestValidateToken_DefaultIsPassthrough(t *testing.T) {
	// NOT parallel: this reads the package-level validateTokenFn which
	// TestAuthenticateFromHeader subtests mutate via setValidateToken.
	claims, err := ValidateToken("anything")
	if err != nil {
		t.Fatalf("default ValidateToken must not error: %v", err)
	}
	if claims != nil {
		t.Fatalf("default ValidateToken must return nil claims, got %+v", claims)
	}
}

// AuthInterceptor must panic at construction when no provider is
// configured AND no dev opt-in is set. Per-request rejection (the old
// behavior) silently broke pack-installed deployments because the stub
// would reject before the pack's interceptor ran.
func TestAuthInterceptor_UnconfiguredPanics(t *testing.T) {
	// NOT parallel: mutates package-level config flags and env vars that
	// other subtests in this file also depend on.
	authMu.Lock()
	origFn, origConfigured, origExternal := validateTokenFn, validatorConfigured, externalAuthRegistered
	validateTokenFn = defaultValidateToken
	validatorConfigured = false
	externalAuthRegistered = false
	authMu.Unlock()
	t.Setenv("ENVIRONMENT", "production")
	t.Setenv("AUTH_MODE", "")
	t.Cleanup(func() {
		authMu.Lock()
		validateTokenFn = origFn
		validatorConfigured = origConfigured
		externalAuthRegistered = origExternal
		authMu.Unlock()
	})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("AuthInterceptor must panic when unconfigured outside dev mode")
		}
	}()
	_ = AuthInterceptor()
}

// MarkExternalAuth puts the stub in passthrough mode — the pack's own
// interceptor in the chain is the source of truth and the stub must not
// reject or even inspect the Authorization header.
func TestAuthInterceptor_ExternalAuthIsPassthrough(t *testing.T) {
	withExternalAuth(t)

	ic := AuthInterceptor()
	if ic == nil {
		t.Fatal("AuthInterceptor must not return nil")
	}
	// In passthrough mode, WrapUnary returns the next func untouched —
	// no inspection of the request. Confirm via identity: the wrapped
	// func must run unconditionally without checking auth.
	called := false
	next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	})
	wrapped := ic.WrapUnary(next)
	if wrapped == nil {
		t.Fatal("WrapUnary must return a non-nil UnaryFunc")
	}
	// Same address as `next` is the strongest signal of passthrough, but
	// Go function-value equality isn't guaranteed; assert behavior instead.
	_, _ = wrapped(context.Background(), nil)
	if !called {
		t.Fatal("passthrough WrapUnary must invoke next unconditionally")
	}
}

// Dev mode (ENVIRONMENT=development) is an explicit opt-in to running
// without an auth provider — the constructor must NOT panic, and the
// interceptor must be a passthrough.
func TestAuthInterceptor_DevModeIsPassthrough(t *testing.T) {
	authMu.Lock()
	origConfigured, origExternal := validatorConfigured, externalAuthRegistered
	validatorConfigured = false
	externalAuthRegistered = false
	authMu.Unlock()
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("AUTH_MODE", "")
	t.Cleanup(func() {
		authMu.Lock()
		validatorConfigured = origConfigured
		externalAuthRegistered = origExternal
		authMu.Unlock()
	})

	ic := AuthInterceptor() // must not panic
	if ic == nil {
		t.Fatal("AuthInterceptor must not return nil in dev mode")
	}
	called := false
	wrapped := ic.WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	})
	_, _ = wrapped(context.Background(), nil)
	if !called {
		t.Fatal("dev-mode WrapUnary must invoke next unconditionally")
	}
}

// AUTH_MODE=none is the explicit production opt-out — same passthrough
// behavior as dev mode, but the operator stated it deliberately rather
// than relying on the dev-mode default.
func TestAuthInterceptor_AuthModeNoneIsPassthrough(t *testing.T) {
	authMu.Lock()
	origConfigured, origExternal := validatorConfigured, externalAuthRegistered
	validatorConfigured = false
	externalAuthRegistered = false
	authMu.Unlock()
	t.Setenv("ENVIRONMENT", "production")
	t.Setenv("AUTH_MODE", "none")
	t.Cleanup(func() {
		authMu.Lock()
		validatorConfigured = origConfigured
		externalAuthRegistered = origExternal
		authMu.Unlock()
	})

	ic := AuthInterceptor() // must not panic
	if ic == nil {
		t.Fatal("AuthInterceptor must not return nil with AUTH_MODE=none")
	}
}

// When SetTokenValidator installed a real validator, the interceptor must
// be the source of truth — NOT a passthrough — and exercise the
// authenticateFromHeader path on the request.
func TestAuthInterceptor_ValidatorConfigured(t *testing.T) {
	setValidateToken(t, func(string) (*Claims, error) {
		return &Claims{UserID: "u1"}, nil
	})
	ic := AuthInterceptor()
	if ic == nil {
		t.Fatal("AuthInterceptor must not return nil when configured")
	}
	// WrapUnary returns a wrapping function (not identity); we can't check
	// for exact passthrough here without spinning up a Connect request.
	// The authenticateFromHeader path is exercised directly by
	// TestAuthenticateFromHeader.
	if ic.WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) { return nil, nil }) == nil {
		t.Fatal("WrapUnary must return a non-nil UnaryFunc")
	}
}