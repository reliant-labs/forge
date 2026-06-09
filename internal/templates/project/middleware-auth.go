//go:build ignore

package middleware

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"connectrpc.com/connect"
)

// unauthenticatedProcedures is an explicit allow-list of RPC procedures that
// bypass the auth interceptor. Entries must be full procedure strings of the
// form "/package.Service/Method" — no substring matching.
//
// Extend this set to expose additional unauthenticated endpoints (e.g. public
// health, readiness, or version RPCs). Keeping the list exact prevents
// accidentally bypassing auth for any procedure whose name happens to contain
// a matching substring (e.g. a user-defined "HealthReport" RPC).
var unauthenticatedProcedures = map[string]struct{}{
	"/grpc.health.v1.Health/Check": {},
	"/grpc.health.v1.Health/Watch": {},
}

// isUnauthenticatedProcedure reports whether the given full procedure string
// is in the explicit allow-list and should skip authentication.
func isUnauthenticatedProcedure(procedure string) bool {
	_, ok := unauthenticatedProcedures[procedure]
	return ok
}

// AuthInterceptor creates a Connect RPC authentication interceptor that
// handles both unary and streaming RPCs.
//
// Three startup modes (chosen at construction time, not per-request):
//
//  1. **Passthrough (pack-installed or external)**: when a pack — e.g.
//     jwt-auth, clerk, firebase — has called [MarkExternalAuth] during
//     its Init, OR when the project has called [SetTokenValidator] with
//     `nil`, this interceptor becomes a no-op identity. The pack's own
//     interceptor (added alongside via cmd/server.go's interceptor chain)
//     is the source of truth.
//
//  2. **Stub validates**: when the project has called [SetTokenValidator]
//     with a real validator, this interceptor extracts the Bearer token,
//     calls the validator, and attaches claims to context.
//
//  3. **Dev passthrough**: when ENVIRONMENT=development (or "dev") or
//     AUTH_MODE=none, this interceptor is a no-op identity even with no
//     validator and no pack. The frictionless local-dev path.
//
// If NONE of (1)-(3) apply, [AuthInterceptor] panics at construction. A
// production server with no auth provider configured is always a bug;
// failing loudly at startup is safer than silently accepting every
// request or silently rejecting every request.
func AuthInterceptor() connect.Interceptor {
	mode := resolveAuthMode()
	if mode == authModeUnconfigured {
		panic("middleware.AuthInterceptor: no auth provider configured — " +
			"install a pack (e.g. `forge pack install jwt-auth`), call " +
			"middleware.SetTokenValidator with a real validator, or set " +
			"AUTH_MODE=none (or ENVIRONMENT=development) to explicitly " +
			"run without authentication. See pkg/middleware/auth.go for details.")
	}
	return &authInterceptor{passthrough: mode == authModePassthrough}
}

type authInterceptor struct {
	// passthrough is set at construction time. When true, this interceptor
	// does not look at the Authorization header — the pack interceptor
	// later in the chain (or the dev opt-in) is responsible for auth.
	passthrough bool
}

func (a *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	if a.passthrough {
		return next
	}
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if isUnauthenticatedProcedure(req.Spec().Procedure) {
			return next(ctx, req)
		}

		ctx, err := authenticateFromHeader(ctx, req.Header().Get("Authorization"))
		if err != nil {
			return nil, err
		}

		return next(ctx, req)
	}
}

func (a *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next // Client-side, no server auth needed
}

func (a *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	if a.passthrough {
		return next
	}
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if isUnauthenticatedProcedure(conn.Spec().Procedure) {
			return next(ctx, conn)
		}

		ctx, err := authenticateFromHeader(ctx, conn.RequestHeader().Get("Authorization"))
		if err != nil {
			return err
		}

		return next(ctx, conn)
	}
}

// authenticateFromHeader extracts and validates a Bearer token from the
// Authorization header. If no token is present, the context is returned
// unchanged (unauthenticated). If a token is present and valid, claims
// are added to the context.
func authenticateFromHeader(ctx context.Context, authorization string) (context.Context, error) {
	if authorization == "" {
		return ctx, nil
	}

	token := strings.TrimPrefix(authorization, "Bearer ")
	if token == authorization {
		return ctx, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid authorization format"))
	}

	claims, err := ValidateToken(token)
	if err != nil {
		return ctx, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid token: %w", err))
	}

	return ContextWithClaims(ctx, claims), nil
}

// VerifyAuth checks if the user has the required roles.
// It checks both the single Role field and the Roles slice for a match.
func VerifyAuth(ctx context.Context, requiredRoles ...string) error {
	claims, ok := ClaimsFromContext(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no authentication claims found"))
	}

	if len(requiredRoles) == 0 {
		return nil
	}

	// Check if user has any of the required roles
	for _, requiredRole := range requiredRoles {
		// Check the single Role field
		if claims.Role == requiredRole {
			return nil
		}
		// Check the Roles slice
		for _, userRole := range claims.Roles {
			if userRole == requiredRole {
				return nil
			}
		}
	}

	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("insufficient permissions"))
}

// authMu guards the package-level configuration flags below. All mutation
// happens during startup (before the interceptor is constructed), but the
// mutex makes the data race detector happy when tests swap validators
// across subtests via t.Cleanup.
var authMu sync.Mutex

// validateTokenFn is the package-level token validator. It is a variable
// (rather than a function) so projects with real authentication can swap
// it in during bootstrap via [SetTokenValidator], and so tests can install
// a stub without resorting to build tags or linker tricks.
//
// The default returns (nil, nil) — a no-op. When this default is in place
// AND no external auth has been registered AND no dev opt-in is set, the
// [AuthInterceptor] constructor panics rather than letting the default run.
// See [resolveAuthMode] for the decision table.
var validateTokenFn func(string) (*Claims, error) = defaultValidateToken

// validatorConfigured reports whether [SetTokenValidator] has been called
// with a non-nil validator. Setting this to true switches the stub into
// "validate every request" mode.
var validatorConfigured bool

// externalAuthRegistered reports whether a pack (or hand-written setup
// code) has called [MarkExternalAuth] to indicate that auth is handled by
// another interceptor in the chain. This switches the stub into pure
// passthrough mode regardless of ENVIRONMENT.
var externalAuthRegistered bool

// defaultValidateToken is the no-op default. It is only ever reached if
// the interceptor was constructed in passthrough mode (see resolveAuthMode),
// in which case the per-request handler skips calling it entirely. It is
// also called directly by tests against the package — keep the signature
// stable.
func defaultValidateToken(string) (*Claims, error) { return nil, nil }

// SetTokenValidator installs a real token validator. Call this during
// startup, before constructing the interceptor (i.e. before cmd/server.go
// builds the interceptor chain). Passing nil resets to the no-op default
// but does NOT clear the external-auth registration.
func SetTokenValidator(fn func(string) (*Claims, error)) {
	authMu.Lock()
	defer authMu.Unlock()
	if fn == nil {
		validateTokenFn = defaultValidateToken
		validatorConfigured = false
		return
	}
	validateTokenFn = fn
	validatorConfigured = true
}

// MarkExternalAuth signals that an auth provider (a pack, or hand-rolled
// code) has installed its own Connect interceptor alongside this one. The
// stub then becomes a pure passthrough so the external interceptor is the
// sole source of truth.
//
// Packs call this from their Init function (e.g. jwt-auth's Init). Hand-
// rolled setups can call it directly when adding a custom auth interceptor
// to the chain.
func MarkExternalAuth() {
	authMu.Lock()
	defer authMu.Unlock()
	externalAuthRegistered = true
}

// authMode captures the resolved behavior of [AuthInterceptor] at
// construction time.
type authMode int

const (
	authModeUnconfigured authMode = iota
	authModeValidate
	authModePassthrough
)

// resolveAuthMode reads the package-level config and the environment to
// decide which mode [AuthInterceptor] should run in. Decision order:
//  1. A real validator was registered → validate
//  2. External auth was registered (pack alongside) → passthrough
//  3. AUTH_MODE=none → passthrough (explicit opt-out)
//  4. ENVIRONMENT=development|dev → passthrough (dev ergonomics)
//  5. Otherwise → unconfigured (constructor panics)
func resolveAuthMode() authMode {
	authMu.Lock()
	defer authMu.Unlock()
	switch {
	case validatorConfigured:
		return authModeValidate
	case externalAuthRegistered:
		return authModePassthrough
	case strings.EqualFold(os.Getenv("AUTH_MODE"), "none"):
		return authModePassthrough
	}
	switch strings.ToLower(os.Getenv("ENVIRONMENT")) {
	case "development", "dev":
		return authModePassthrough
	}
	return authModeUnconfigured
}

// ValidateToken validates a bearer token by delegating to validateTokenFn.
// Projects with real authentication should replace validateTokenFn via
// [SetTokenValidator] during startup.
func ValidateToken(token string) (*Claims, error) {
	authMu.Lock()
	fn := validateTokenFn
	authMu.Unlock()
	return fn(token)
}