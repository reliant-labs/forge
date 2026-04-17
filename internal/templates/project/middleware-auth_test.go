//go:build ignore

package middleware

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
)

// setValidateToken swaps the package-level token validator for the duration
// of a single subtest and restores it via t.Cleanup. This is the only safe
// way to exercise the real middleware without stubbing its call tree.
func setValidateToken(t *testing.T, fn func(string) (*Claims, error)) {
	t.Helper()
	orig := validateTokenFn
	validateTokenFn = fn
	t.Cleanup(func() { validateTokenFn = orig })
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

// Ensure the default stub ValidateToken still surfaces a clear error so
// callers that forget to install a real validator don't produce a
// confusing nil-panic further down the stack.
func TestValidateToken_DefaultIsNotConfigured(t *testing.T) {
	// NOT parallel: this reads the package-level validateTokenFn which
	// TestAuthenticateFromHeader subtests mutate via setValidateToken.
	_, err := ValidateToken("anything")
	if err == nil {
		t.Fatal("default ValidateToken must return an error")
	}
	if got := fmt.Sprint(err); got == "" {
		t.Fatal("default ValidateToken error must not be empty")
	}
}

// The AuthInterceptor itself is a trivial dispatch over its collaborators:
// it defers to isUnauthenticatedProcedure (tested above) and
// authenticateFromHeader (tested above). TestAuthInterceptor_Type provides
// a minimal smoke-test that the constructor returns a non-nil
// connect.Interceptor satisfying the interface — exercising its
// branches end-to-end would require spinning up a real Connect
// server, which is covered at the service integration-test layer.
func TestAuthInterceptor_Type(t *testing.T) {
	t.Parallel()
	ic := AuthInterceptor()
	if ic == nil {
		t.Fatal("AuthInterceptor must not return nil")
	}
	// WrapUnary must return a non-nil function — checking this guards
	// against future refactors dropping the wrapping accidentally.
	if ic.WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) { return nil, nil }) == nil {
		t.Fatal("WrapUnary must return a non-nil UnaryFunc")
	}
}