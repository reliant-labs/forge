//go:build ignore

package middleware

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/trace"
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
			slog.String("procedure", req.Spec().Procedure),
			slog.String("duration", time.Since(start).String()),
		}
		if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.HasTraceID() {
			attrs = append(attrs, slog.String("trace_id", spanCtx.TraceID().String()))
		}
		if err != nil {
			attrs = append(attrs, slog.Any("error", err))
			i.logger.LogAttrs(ctx, slog.LevelError, "rpc error", attrs...)
		} else {
			i.logger.LogAttrs(ctx, slog.LevelInfo, "rpc ok", attrs...)
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
			slog.String("procedure", conn.Spec().Procedure),
			slog.String("duration", time.Since(start).String()),
		}
		if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.HasTraceID() {
			attrs = append(attrs, slog.String("trace_id", spanCtx.TraceID().String()))
		}
		if err != nil {
			attrs = append(attrs, slog.Any("error", err))
			i.logger.LogAttrs(ctx, slog.LevelError, "stream error", attrs...)
		} else {
			i.logger.LogAttrs(ctx, slog.LevelInfo, "stream ok", attrs...)
		}

		return err
	})
}
