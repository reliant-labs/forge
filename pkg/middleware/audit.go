package middleware

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
)

// AuditInterceptor creates a Connect interceptor that produces audit log
// entries for every RPC call. Audit logs capture: who made the call
// (user ID, email), what procedure was called, when, the result
// (success/error code), and duration.
//
// Audit records are emitted with log_type=audit on a child logger, so
// they can be routed to a dedicated audit sink (separate file, SIEM,
// compliance database) independent of operational logs. The record
// message is "audit.event" — INFO on success, WARN on error.
//
// claimsFrom is the project's ClaimsFromContext (the project owns the
// claims context key — see [ClaimsLookup]). When nil, or when no claims
// are present (unauthenticated request), the user is logged as
// "anonymous".
//
// NOTE: the audit-log pack enhances this interceptor with database
// persistence. Install it with `forge pack install audit-log` for
// queryable audit history.
func AuditInterceptor(logger *slog.Logger, claimsFrom ClaimsLookup) connect.Interceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return &auditInterceptor{
		logger:     logger.With("log_type", "audit"),
		claimsFrom: claimsFrom,
	}
}

type auditInterceptor struct {
	logger     *slog.Logger
	claimsFrom ClaimsLookup
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
		slog.Duration("duration", time.Since(start)),
		slog.Time("timestamp", start),
	}

	// Extract user identity from auth claims if available.
	var identified bool
	if a.claimsFrom != nil {
		if claims, ok := a.claimsFrom(ctx); ok && claims != nil {
			attrs = append(attrs,
				slog.String("user_id", claims.UserID),
				slog.String("email", claims.Email),
			)
			identified = true
		}
	}
	if !identified {
		attrs = append(attrs, slog.String("user_id", "anonymous"))
	}

	if err != nil {
		code := connect.CodeOf(err)
		attrs = append(attrs,
			slog.String("status", "error"),
			slog.String("code", code.String()),
			slog.String("error", err.Error()),
		)
		a.logger.LogAttrs(ctx, slog.LevelWarn, "audit.event", attrs...)
	} else {
		attrs = append(attrs, slog.String("status", "ok"))
		a.logger.LogAttrs(ctx, slog.LevelInfo, "audit.event", attrs...)
	}
}
