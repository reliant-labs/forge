//go:build ignore

package middleware

import (
	"context"

	"github.com/reliant-labs/forge/pkg/auth"
)

type claimsContextKey struct{}

var userClaimsKey = claimsContextKey{}

// Claims is the canonical claims type used throughout the application.
// It is a type alias for auth.Claims so library code (the generated auth
// interceptor in auth_gen.go and the tenant interceptor) and project code
// share the same type.
//
// To extend Claims with project-specific fields, prefer adding them to
// pkg/auth.Claims upstream. If you need a project-local extension, replace
// this alias with a struct that embeds auth.Claims and update auth_gen.go's
// withClaims callback accordingly — but note this disables the alias-based
// re-export and you must propagate your custom struct everywhere middleware
// today expects *Claims.
type Claims = auth.Claims

// ClaimsFromContext retrieves user claims from the context. Returns nil, false
// if no claims are present (e.g., unauthenticated request).
func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	claims, ok := ctx.Value(userClaimsKey).(*Claims)
	return claims, ok
}

// ContextWithClaims returns a new context with the given claims attached.
func ContextWithClaims(ctx context.Context, claims *Claims) context.Context {
	return context.WithValue(ctx, userClaimsKey, claims)
}
