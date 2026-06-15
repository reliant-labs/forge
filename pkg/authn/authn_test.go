package authn

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/auth"
)

// Test claims-stash helpers standing in for the project-owned context
// key (pkg/middleware in a real scaffold).
type testClaimsKey struct{}

func testContextWithClaims(ctx context.Context, claims *auth.Claims) context.Context {
	return context.WithValue(ctx, testClaimsKey{}, claims)
}

func testClaimsFromContext(ctx context.Context) (*auth.Claims, bool) {
	c, ok := ctx.Value(testClaimsKey{}).(*auth.Claims)
	return c, ok
}

func healthAllowList() map[string]struct{} {
	return map[string]struct{}{
		"/grpc.health.v1.Health/Check": {},
		"/grpc.health.v1.Health/Watch": {},
	}
}

// validatePolicy returns a Policy in Validate mode with the given
// validator and the test claims stash.
func validatePolicy(validate func(string) (*auth.Claims, error)) Policy {
	return Policy{
		ValidatorConfigured: true,
		Validate:            validate,
		Unauthenticated:     healthAllowList(),
		ContextWithClaims:   testContextWithClaims,
	}
}

// H2 contract: refusal-without-validator. NewInterceptor must return an
// error at construction when no provider is configured AND no explicit
// opt-out is set — startup aborts instead of serving with fictional
// auth. Per-request rejection (the historical behavior) silently broke
// pack-installed deployments because the stub rejected before the
// pack's interceptor ran.
func TestNewInterceptor_UnconfiguredErrors(t *testing.T) {
	t.Setenv("AUTH_MODE", "")
	ic, err := NewInterceptor(Policy{})
	if err == nil {
		t.Fatal("NewInterceptor must error when unconfigured outside dev mode")
	}
	if ic != nil {
		t.Fatal("NewInterceptor must return a nil interceptor alongside the error")
	}
}

// H2 contract: AUTH_MODE=none is the explicit production opt-out — same
// passthrough behavior as dev mode, but the operator stated it
// deliberately rather than relying on the dev-mode default.
func TestNewInterceptor_AuthModeNoneIsPassthrough(t *testing.T) {
	t.Setenv("AUTH_MODE", "none")
	ic, err := NewInterceptor(Policy{})
	if err != nil {
		t.Fatalf("NewInterceptor with AUTH_MODE=none must not error: %v", err)
	}
	assertPassthroughUnary(t, ic)
}

// H2 contract: config.Mode injection. Dev mode — injected from the
// typed config via Policy.DevMode, NOT read from the environment here —
// is an explicit opt-in to running without an auth provider:
// construction succeeds and the interceptor is a passthrough.
func TestNewInterceptor_DevModeIsPassthrough(t *testing.T) {
	t.Setenv("AUTH_MODE", "")
	ic, err := NewInterceptor(Policy{DevMode: true})
	if err != nil {
		t.Fatalf("NewInterceptor in dev mode must not error: %v", err)
	}
	assertPassthroughUnary(t, ic)
}

// External auth puts the interceptor in passthrough mode — the pack's
// own interceptor in the chain is the source of truth and this one must
// not reject or even inspect the Authorization header.
func TestNewInterceptor_ExternalAuthIsPassthrough(t *testing.T) {
	t.Setenv("AUTH_MODE", "")
	ic, err := NewInterceptor(Policy{ExternalAuth: true})
	if err != nil {
		t.Fatalf("NewInterceptor with external auth must not error: %v", err)
	}
	assertPassthroughUnary(t, ic)
}

// assertPassthroughUnary asserts the interceptor invokes next
// unconditionally (no header inspection, nil request tolerated).
func assertPassthroughUnary(t *testing.T, ic connect.Interceptor) {
	t.Helper()
	if ic == nil {
		t.Fatal("interceptor must not be nil")
	}
	called := false
	next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	})
	wrapped := ic.WrapUnary(next)
	if wrapped == nil {
		t.Fatal("WrapUnary must return a non-nil UnaryFunc")
	}
	if _, err := wrapped(context.Background(), nil); err != nil {
		t.Fatalf("passthrough must not error: %v", err)
	}
	if !called {
		t.Fatal("passthrough WrapUnary must invoke next unconditionally")
	}
}

// When a validator is configured, the interceptor must be the source of
// truth — NOT a passthrough — even in dev mode or under AUTH_MODE=none
// (decision order: validator wins).
func TestNewInterceptor_ValidatorWinsOverOptOuts(t *testing.T) {
	t.Setenv("AUTH_MODE", "none")
	p := validatePolicy(func(string) (*auth.Claims, error) { return &auth.Claims{UserID: "u1"}, nil })
	p.DevMode = true
	ic, err := NewInterceptor(p)
	if err != nil {
		t.Fatalf("NewInterceptor with validator must not error: %v", err)
	}
	if _, ok := ic.(*interceptor); !ok {
		t.Fatalf("validator-configured policy must resolve to Validate mode, got %T", ic)
	}
}

// Validate mode without the claims stash (or validator func) is a
// construction bug, surfaced at boot.
func TestNewInterceptor_ValidateModeRequiresHooks(t *testing.T) {
	t.Setenv("AUTH_MODE", "")
	if _, err := NewInterceptor(Policy{ValidatorConfigured: true}); err == nil {
		t.Fatal("ValidatorConfigured without Validate must refuse construction")
	}
	if _, err := NewInterceptor(Policy{
		ValidatorConfigured: true,
		Validate:            func(string) (*auth.Claims, error) { return nil, nil },
	}); err == nil {
		t.Fatal("Validate mode without ContextWithClaims must refuse construction")
	}
}

// H2 contracts: 401-on-empty-Authorization, malformed header, invalid
// token, and the happy path stashing claims via the project hook. The
// authenticate collaborator is tested directly because
// connect.AnyRequest is a sealed interface — only the generated Connect
// shim can construct one with an arbitrary Procedure.
func TestAuthenticate(t *testing.T) {
	t.Parallel()

	validClaims := &auth.Claims{UserID: "user-1", Email: "u@example.com"}

	tests := []struct {
		name          string
		authorization string
		validate      func(string) (*auth.Claims, error)
		wantErrCode   connect.Code // 0 → success
		wantClaims    bool
	}{
		{
			// The explicit allow-list (checked by the callers) is the
			// ONLY unauthenticated path; a missing header on any other
			// procedure is rejected, never silently passed through.
			name:        "missing token is rejected",
			wantErrCode: connect.CodeUnauthenticated,
		},
		{
			name:          "malformed authorization header is rejected",
			authorization: "Token abc", // not a Bearer token
			wantErrCode:   connect.CodeUnauthenticated,
		},
		{
			name:          "invalid bearer token is rejected",
			authorization: "Bearer bad",
			validate:      func(string) (*auth.Claims, error) { return nil, errors.New("bad sig") },
			wantErrCode:   connect.CodeUnauthenticated,
		},
		{
			name:          "valid bearer token attaches claims",
			authorization: "Bearer good",
			validate:      func(string) (*auth.Claims, error) { return validClaims, nil },
			wantClaims:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			validate := tc.validate
			if validate == nil {
				validate = func(string) (*auth.Claims, error) { return nil, nil }
			}
			a := &interceptor{policy: validatePolicy(validate)}

			ctx, err := a.authenticate(context.Background(), tc.authorization)

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
			got, ok := testClaimsFromContext(ctx)
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

// H2 contract: allow-list-only-unauthenticated, exact matching.
// Substring-containing procedures must not match — the allow-list is
// deliberately exact to prevent accidental bypass.
func TestAllowUnauthenticated_ExactMatchOnly(t *testing.T) {
	t.Parallel()
	a := &interceptor{policy: validatePolicy(func(string) (*auth.Claims, error) { return nil, nil })}

	if !a.allowUnauthenticated("/grpc.health.v1.Health/Check") {
		t.Fatal("health Check should be allow-listed")
	}
	if !a.allowUnauthenticated("/grpc.health.v1.Health/Watch") {
		t.Fatal("health Watch should be allow-listed")
	}
	if a.allowUnauthenticated("/grpc.health.v1.Health/Report") {
		t.Fatal("Health/Report is not in the allow-list")
	}
	if a.allowUnauthenticated("/demo.v1.Service/HealthCheck") {
		t.Fatal("user-defined HealthCheck must not be matched by substring")
	}
	if a.allowUnauthenticated("") {
		t.Fatal("empty procedure must not match")
	}
}

// The enricher hook runs after validation, can rewrite claims, and a
// failure rejects the request (CodeUnauthenticated unless the hook
// returned an explicit *connect.Error).
func TestAuthenticate_EnricherHook(t *testing.T) {
	t.Parallel()

	base := &auth.Claims{UserID: "u1"}
	p := validatePolicy(func(string) (*auth.Claims, error) { return base, nil })
	p.Enrich = func(ctx context.Context, c *auth.Claims) (*auth.Claims, error) {
		enriched := *c
		enriched.Roles = []string{"admin"}
		return &enriched, nil
	}
	a := &interceptor{policy: p}

	ctx, err := a.authenticate(context.Background(), "Bearer good")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := testClaimsFromContext(ctx)
	if !ok || len(got.Roles) != 1 || got.Roles[0] != "admin" {
		t.Fatalf("enricher output not stashed: %+v", got)
	}

	// Plain-error failure → CodeUnauthenticated.
	p.Enrich = func(context.Context, *auth.Claims) (*auth.Claims, error) {
		return nil, errors.New("user row missing")
	}
	a = &interceptor{policy: p}
	if _, err := a.authenticate(context.Background(), "Bearer good"); err == nil {
		t.Fatal("enricher failure must reject the request")
	} else {
		var cerr *connect.Error
		if !errors.As(err, &cerr) || cerr.Code() != connect.CodeUnauthenticated {
			t.Fatalf("want Unauthenticated, got %v", err)
		}
	}

	// Explicit connect.Error is preserved verbatim.
	p.Enrich = func(context.Context, *auth.Claims) (*auth.Claims, error) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("suspended"))
	}
	a = &interceptor{policy: p}
	if _, err := a.authenticate(context.Background(), "Bearer good"); err == nil {
		t.Fatal("enricher connect.Error must reject the request")
	} else {
		var cerr *connect.Error
		if !errors.As(err, &cerr) || cerr.Code() != connect.CodePermissionDenied {
			t.Fatalf("want PermissionDenied preserved, got %v", err)
		}
	}
}

// Dev-claims passthrough: with DevClaims set, dev/AUTH_MODE=none
// passthrough still invokes next unconditionally but attaches the
// synthetic principal so claim-reading handlers keep working.
func TestNewInterceptor_DevClaims(t *testing.T) {
	t.Setenv("AUTH_MODE", "")
	dev := &auth.Claims{UserID: "dev-user", Role: "admin"}
	ic, err := NewInterceptor(Policy{
		DevMode:           true,
		DevClaims:         func() *auth.Claims { return dev },
		ContextWithClaims: testContextWithClaims,
	})
	if err != nil {
		t.Fatalf("NewInterceptor with dev claims must not error: %v", err)
	}

	var seen *auth.Claims
	wrapped := ic.WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		seen, _ = testClaimsFromContext(ctx)
		return nil, nil
	})
	if _, err := wrapped(context.Background(), nil); err != nil {
		t.Fatalf("dev-claims passthrough must not error: %v", err)
	}
	if seen == nil || seen.UserID != "dev-user" {
		t.Fatalf("dev claims not attached, got %+v", seen)
	}

	// DevClaims requires the claims stash.
	if _, err := NewInterceptor(Policy{
		DevMode:   true,
		DevClaims: func() *auth.Claims { return dev },
	}); err == nil {
		t.Fatal("DevClaims without ContextWithClaims must refuse construction")
	}

	// External auth ignores DevClaims — the external provider owns claims.
	ic, err = NewInterceptor(Policy{
		ExternalAuth:      true,
		DevClaims:         func() *auth.Claims { return dev },
		ContextWithClaims: testContextWithClaims,
	})
	if err != nil {
		t.Fatalf("external auth construction failed: %v", err)
	}
	if _, ok := ic.(passthrough); !ok {
		t.Fatalf("external auth must be a pure passthrough, got %T", ic)
	}
}
