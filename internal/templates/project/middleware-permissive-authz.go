//go:build ignore

package middleware

import "context"

// DevAuthorizer is an allow-all Authorizer intended for use in local
// development only. Production authorizers deny by default (fail-closed);
// DevAuthorizer is the opt-in dev-ergonomics counterpart selected when
// cfg.Environment == "development".
//
// DO NOT use this in production. Bootstrap guards its use behind an explicit
// config value and logs a WARN at startup when it is active.
type DevAuthorizer struct{}

// CanAccess implements Authorizer.CanAccess. Always returns nil (allow).
func (DevAuthorizer) CanAccess(context.Context, string) error { return nil }

// Can implements Authorizer.Can. Always returns nil (allow).
func (DevAuthorizer) Can(context.Context, *Claims, string, string) error { return nil }
