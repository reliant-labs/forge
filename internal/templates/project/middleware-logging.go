//go:build ignore

package middleware

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
)

// loggingInterceptor implements connect.Interceptor with request logging
// for both unary and streaming RPCs.
type loggingInterceptor struct {
	logger *slog.Logger
}

// LoggingInterceptor creates a Connect RPC interceptor that logs all calls.
// It logs the procedure name, duration, and any errors.
func LoggingInterceptor(logger *slog.Logger) connect.Interceptor {
	return &loggingInterceptor{logger: logger}
}

func (i *loggingInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()

		resp, err := next(ctx, req)

		attrs := []slog.Attr{
			ProcedureAttr(req.Spec().Procedure),
			DurationAttr(time.Since(start)),
		}
		if rid := RequestIDFromContext(ctx); rid != "" {
			attrs = append(attrs, RequestIDAttr(rid))
		} else if rid := req.Header().Get(RequestIDHeader); rid != "" {
			// Fall back to the raw header in case the request-id middleware
			// was not wired at the HTTP layer. This keeps logs correlatable
			// even in partial deployments.
			attrs = append(attrs, RequestIDAttr(rid))
		}
		if err != nil {
			attrs = append(attrs, slog.Any("error", err))
			LogRequestFailed(ctx, i.logger, attrs...)
		} else {
			LogRequestCompleted(ctx, i.logger, attrs...)
		}

		return resp, err
	})
}

func (i *loggingInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *loggingInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return connect.StreamingHandlerFunc(func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()

		err := next(ctx, conn)

		attrs := []slog.Attr{
			ProcedureAttr(conn.Spec().Procedure),
			DurationAttr(time.Since(start)),
		}
		if rid := RequestIDFromContext(ctx); rid != "" {
			attrs = append(attrs, RequestIDAttr(rid))
		} else if rid := conn.RequestHeader().Get(RequestIDHeader); rid != "" {
			attrs = append(attrs, RequestIDAttr(rid))
		}
		if err != nil {
			attrs = append(attrs, slog.Any("error", err))
			LogStreamFailed(ctx, i.logger, attrs...)
		} else {
			LogStreamCompleted(ctx, i.logger, attrs...)
		}

		return err
	})
}