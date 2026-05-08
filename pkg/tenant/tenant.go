// Package tenant provides multi-tenant context helpers and a Connect
// interceptor that extracts a tenant ID from authenticated [auth.Claims] and
// stores it in the request context.
//
// The interceptor must run AFTER the auth interceptor in the chain. Generated
// forge projects wire it via a thin shim in pkg/middleware/tenant_gen.go.
package tenant

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/auth"
)

// Config configures the tenant interceptor.
type Config struct {
	// ClaimField is the JWT claim from which the tenant ID is read
	// (e.g. "org_id", "tenant_id"). Defaults to "org_id".
	ClaimField string

	// ColumnName is the database column used for tenant scoping. Stored on
	// Config so callers can introspect it; the interceptor itself does not
	// touch the database. Defaults to "org_id".
	ColumnName string

	// Optional, when true, lets requests proceed when claims are present but
	// the tenant claim is empty. The default (false) matches the legacy
	// template behaviour: a present-but-empty tenant claim returns
	// PermissionDenied.
	Optional bool

	// Extract, when non-nil, fully overrides the default tenant-claim
	// extraction. Useful for tenant claims that aren't on Claims directly.
	Extract func(claims *auth.Claims, field string) string
}

// EffectiveClaimField returns ClaimField or the "org_id" default.
func (c Config) EffectiveClaimField() string {
	if c.ClaimField == "" {
		return "org_id"
	}
	return c.ClaimField
}

// EffectiveColumnName returns ColumnName or the "org_id" default.
func (c Config) EffectiveColumnName() string {
	if c.ColumnName == "" {
		return "org_id"
	}
	return c.ColumnName
}

type tenantIDKey struct{}

// WithTenantID returns a new context with the tenant ID set.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantIDKey{}, tenantID)
}

// FromContext returns the tenant ID stored in the context (empty string when absent).
func FromContext(ctx context.Context) string {
	v, _ := ctx.Value(tenantIDKey{}).(string)
	return v
}

// Require returns the tenant ID from context or an error if not present.
func Require(ctx context.Context) (string, error) {
	id := FromContext(ctx)
	if id == "" {
		return "", fmt.Errorf("tenant ID not found in context")
	}
	return id, nil
}

// ExtractClaim is the default claim-to-tenant-id mapping. It checks the
// requested field first, falls back to OrgID for unknown fields (preserves
// the legacy template behaviour).
func ExtractClaim(claims *auth.Claims, field string) string {
	if claims == nil {
		return ""
	}
	switch field {
	case "org_id":
		return claims.OrgID
	case "sub", "subject", "user_id":
		return claims.UserID
	case "email":
		return claims.Email
	case "role":
		return claims.Role
	default:
		return claims.OrgID
	}
}

// ClaimsLookup is the function the project supplies for reading [auth.Claims]
// from the request context. Typically middleware.ClaimsFromContext.
type ClaimsLookup func(context.Context) (*auth.Claims, bool)

// ApplyToContext extracts the tenant ID from claims (read via claimsFromContext)
// and returns a context with the tenant ID set. If claims are not present, the
// context is returned unchanged. If claims are present but the tenant claim is
// empty, ApplyToContext returns a PermissionDenied connect.Error unless
// cfg.Optional is set.
//
// This is the testable core of [NewInterceptor]; the interceptor wraps it with
// the AnyRequest plumbing.
func ApplyToContext(ctx context.Context, cfg Config, claimsFromContext ClaimsLookup) (context.Context, error) {
	claims, ok := claimsFromContext(ctx)
	if !ok || claims == nil {
		// No claims = no tenant context (unauthenticated or skipped endpoints).
		return ctx, nil
	}
	field := cfg.EffectiveClaimField()
	extract := cfg.Extract
	if extract == nil {
		extract = ExtractClaim
	}
	tenantID := extract(claims, field)
	if tenantID == "" {
		if cfg.Optional {
			return ctx, nil
		}
		return ctx, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("missing tenant claim %q in token", field))
	}
	return WithTenantID(ctx, tenantID), nil
}

// NewInterceptor returns a Connect interceptor that extracts the tenant ID
// from authenticated claims and injects it into the request context.
//
// claimsFromContext is the project's claims-from-context helper (typically
// middleware.ClaimsFromContext). When it returns ok=false the request passes
// through unchanged (matching the legacy "no claims = no tenant" semantics).
func NewInterceptor(cfg Config, claimsFromContext ClaimsLookup) connect.UnaryInterceptorFunc {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			newCtx, err := ApplyToContext(ctx, cfg, claimsFromContext)
			if err != nil {
				return nil, err
			}
			return next(newCtx, req)
		})
	})
}
