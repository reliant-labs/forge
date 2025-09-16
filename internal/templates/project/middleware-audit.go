//go:build ignore

package middleware

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
)

// AuditInterceptor creates a Connect interceptor that produces audit log entries
// for every RPC call. Audit logs capture: who made the call (user ID, email),
// what procedure was called, when, the result (success/error code), and duration.
//
// Audit logs are written to a dedicated logger, separate from operational logs,
// so they can be routed to a dedicated audit log sink (e.g., separate file,
// SIEM system, or compliance database).
//
// The interceptor extracts user identity from context using ClaimsFromContext.
// If no claims are present (unauthenticated request), the user is logged as "anonymous".
func AuditInterceptor(logger *slog.Logger) connect.Interceptor {
	audit := logger.With("log_type", "audit")
	return &auditInterceptor{logger: audit}
}

type auditInterceptor struct {
	logger *slog.Logger
}

func (a *auditInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()

		resp, err := next(ctx, req)

		a.logAudit(ctx, req.Spec().Procedure, req.Peer().Addr, start, err)

		return resp, err
	}
}

func (a *auditInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *auditInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()

		err := next(ctx, conn)

		a.logAudit(ctx, conn.Spec().Procedure, conn.Peer().Addr, start, err)

		return err
	}
}

func (a *auditInterceptor) logAudit(ctx context.Context, procedure, peerAddr string, start time.Time, err error) {
	attrs := []slog.Attr{
		slog.String("procedure", procedure),
		slog.String("peer", peerAddr),
		slog.String("duration", time.Since(start).String()),
		slog.Time("timestamp", start),
	}

	// Extract user identity from auth claims if available
	claims, ok := ClaimsFromContext(ctx)
	if ok && claims != nil {
		attrs = append(attrs,
			slog.String("user_id", claims.UserID),
			slog.String("email", claims.Email),
		)
	} else {
		attrs = append(attrs, slog.String("user_id", "anonymous"))
	}

	if err != nil {
		code := connect.CodeOf(err)
		attrs = append(attrs,
			slog.String("status", "error"),
			slog.String("code", code.String()),
			slog.String("error", err.Error()),
		)
		a.logger.LogAttrs(ctx, slog.LevelWarn, "audit", attrs...)
	} else {
		attrs = append(attrs, slog.String("status", "ok"))
		a.logger.LogAttrs(ctx, slog.LevelInfo, "audit", attrs...)
	}
}
