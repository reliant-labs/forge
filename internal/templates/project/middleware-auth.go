//go:build ignore

package middleware

import (
	"context"
	"fmt"
	"strings"

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
// If no Bearer token is present, the request proceeds unauthenticated.
func AuthInterceptor() connect.Interceptor {
	return &authInterceptor{}
}

type authInterceptor struct{}

func (a *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
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

// validateTokenFn is the package-level token validator. It is a variable
// (rather than a function) so projects with real authentication can swap
// it in during bootstrap, and so tests can install a stub without resorting
// to build tags or linker tricks.
//
// The default implementation refuses every token with a clear error; leaving
// the middleware wired with a dud validator is always a configuration bug.
var validateTokenFn = func(token string) (*Claims, error) {
	return nil, fmt.Errorf("token validation is not configured")
}

// ValidateToken validates a bearer token by delegating to validateTokenFn.
// Projects with real authentication should replace validateTokenFn during
// startup (e.g. with a JWT or API-key validator).
func ValidateToken(token string) (*Claims, error) {
	return validateTokenFn(token)
}