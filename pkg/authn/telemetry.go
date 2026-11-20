package authn

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// AttrUserID is the telemetry attribute naming the authenticated user: the
// same key pkg/observe's product events use (observe.AttrUserID) and the one
// an issue's "affected users" count reads from its error records.
const AttrUserID = "user.id"

// telemetryUserKey is the context key of the user id the interceptor
// recorded for telemetry. It is separate from the claims stash because a
// project may install its own (Policy.ContextWithClaims), which
// ClaimsFromContext cannot read.
type telemetryUserKey struct{}

// withTelemetryUser records the authenticated user on the request's span and
// in ctx, where UserIDFromContext (and so pkg/observe's log handler) finds
// it. Every record a request logs, and its server span, then name the user,
// so an issue can count the people it reached (NS FG-10). Only the opaque
// subject id is recorded, never an email or a name: it is on every line.
func withTelemetryUser(ctx context.Context, claims *Claims) context.Context {
	if claims == nil || claims.UserID == "" {
		return ctx
	}
	return ContextWithUserID(ctx, claims.UserID)
}

// ContextWithUserID names the user a piece of work is done for: it sets
// user.id on the span in ctx and returns a context whose log records carry
// it (pkg/observe's log handler reads UserIDFromContext). The auth
// interceptor calls it for every authenticated request; a worker or job
// acting on one user's behalf calls it to say whom its errors reached. An
// empty id returns ctx unchanged.
func ContextWithUserID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String(AttrUserID, id))
	return context.WithValue(ctx, telemetryUserKey{}, id)
}

// UserIDFromContext is the authenticated user's id the auth interceptor
// recorded for this request, or "" when the request carried no principal.
func UserIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(telemetryUserKey{}).(string)
	return id
}
