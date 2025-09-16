//go:build ignore

package middleware

import "context"

type claimsContextKey struct{}

var userClaimsKey = claimsContextKey{}

// Claims is the canonical claims type used throughout the application.
// It represents authenticated user claims extracted from a JWT or similar token.
// These fields are used by the auth and audit interceptors. If you need
// additional claims, extend this struct rather than creating a separate type.
type Claims struct {
	UserID string   `json:"user_id"`
	Email  string   `json:"email"`
	OrgID  string   `json:"org_id"`
	Role   string   `json:"role"`
	Roles  []string `json:"roles"`
}

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
