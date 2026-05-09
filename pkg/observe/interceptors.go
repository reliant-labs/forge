package observe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// RequestIDHeader is the canonical correlation header read on inbound
// requests and echoed onto responses. Mirrors the value used by the
// scaffolded pkg/middleware.RequestIDMiddleware (HTTP layer) so the two
// stay consistent end-to-end.
const RequestIDHeader = "X-Request-Id"

type requestIDContextKey struct{}

// ContextWithRequestID attaches id to ctx so downstream handlers and
// log call sites can correlate work across goroutines.
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestIDContextKey{}, id)
}

// RequestIDFromContext returns the request ID stored on ctx (empty when
// absent). Nil-context safe.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(requestIDContextKey{}).(string)
	return v
}

// LoggingInterceptor returns a Connect interceptor that emits one
// slog.Info record per RPC: procedure, duration, request_id, and (on
// failure) error. Matches the shape long-emitted by the scaffolded
// pkg/middleware.LoggingInterceptor — projects that adopt the chain via
// DefaultMiddlewares get the same log records without keeping a copy of
// the interceptor in their tree.
func LoggingInterceptor(logger *slog.Logger) connect.Interceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return &loggingInterceptor{logger: logger}
}

type loggingInterceptor struct {
	logger *slog.Logger
}

func (i *loggingInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		attrs := []slog.Attr{
			slog.String("procedure", req.Spec().Procedure),
			slog.Duration("duration", time.Since(start)),
		}
		if rid := requestIDFromCtxOrHeader(ctx, req.Header()); rid != "" {
			attrs = append(attrs, slog.String("request_id", rid))
		}
		if err != nil {
			attrs = append(attrs, slog.Any("error", err))
			i.logger.LogAttrs(ctx, slog.LevelWarn, "rpc failed", attrs...)
		} else {
			i.logger.LogAttrs(ctx, slog.LevelInfo, "rpc completed", attrs...)
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
			slog.Duration("duration", time.Since(start)),
		}
		if rid := requestIDFromCtxOrHeader(ctx, conn.RequestHeader()); rid != "" {
			attrs = append(attrs, slog.String("request_id", rid))
		}
		if err != nil {
			attrs = append(attrs, slog.Any("error", err))
			i.logger.LogAttrs(ctx, slog.LevelWarn, "stream failed", attrs...)
		} else {
			i.logger.LogAttrs(ctx, slog.LevelInfo, "stream completed", attrs...)
		}
		return err
	})
}

// requestIDFromCtxOrHeader resolves the correlation ID by preferring the
// value already stored on ctx (most accurate — the request-id interceptor
// or HTTP middleware put it there) and falling back to the raw header.
// This makes log records correlatable even in partial deployments where
// only one of the two layers is wired.
func requestIDFromCtxOrHeader(ctx context.Context, header interface{ Get(string) string }) string {
	if rid := RequestIDFromContext(ctx); rid != "" {
		return rid
	}
	if header != nil {
		return header.Get(RequestIDHeader)
	}
	return ""
}

// TracingInterceptor returns a Connect interceptor that creates one
// OpenTelemetry span per RPC. The span name is the full procedure
// ("/service.v1.Foo/Bar"); errors are recorded via span.RecordError +
// span.SetStatus(codes.Error, …).
//
// A nil tracer disables tracing (interceptor is a pass-through). This
// keeps DefaultMiddlewares safe to wire in test harnesses that don't
// configure OTel.
func TracingInterceptor(tracer trace.Tracer) connect.Interceptor {
	if tracer == nil {
		return &noopInterceptor{}
	}
	return &tracingInterceptor{tracer: tracer}
}

type tracingInterceptor struct {
	tracer trace.Tracer
}

func (i *tracingInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, span := i.tracer.Start(ctx, req.Spec().Procedure)
		defer span.End()
		resp, err := next(ctx, req)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		return resp, err
	})
}

func (i *tracingInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *tracingInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return connect.StreamingHandlerFunc(func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, span := i.tracer.Start(ctx, conn.Spec().Procedure)
		defer span.End()
		err := next(ctx, conn)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		return err
	})
}

// MetricsInterceptor returns a Connect interceptor that records three
// OpenTelemetry metrics per RPC:
//
//   - rpc.server.calls    (counter, attribute: procedure)
//   - rpc.server.errors   (counter, attribute: procedure)
//   - rpc.server.duration (histogram seconds, attribute: procedure)
//
// Streaming handlers record one duration sample per stream end. A nil
// meter disables metrics (interceptor is a pass-through), matching
// TracingInterceptor's behaviour for tracer == nil.
func MetricsInterceptor(meter metric.Meter) connect.Interceptor {
	if meter == nil {
		return &noopInterceptor{}
	}
	calls, _ := meter.Int64Counter(
		"rpc.server.calls",
		metric.WithDescription("Total RPC calls"),
	)
	errs, _ := meter.Int64Counter(
		"rpc.server.errors",
		metric.WithDescription("Total RPC errors"),
	)
	dur, _ := meter.Float64Histogram(
		"rpc.server.duration",
		metric.WithDescription("RPC duration in seconds"),
		metric.WithUnit("s"),
	)
	return &metricsInterceptor{
		calls:    calls,
		errs:     errs,
		duration: dur,
	}
}

type metricsInterceptor struct {
	calls    metric.Int64Counter
	errs     metric.Int64Counter
	duration metric.Float64Histogram
}

func (i *metricsInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		attr := metric.WithAttributes(attribute.String("procedure", req.Spec().Procedure))
		if i.calls != nil {
			i.calls.Add(ctx, 1, attr)
		}
		resp, err := next(ctx, req)
		if i.duration != nil {
			i.duration.Record(ctx, time.Since(start).Seconds(), attr)
		}
		if err != nil && i.errs != nil {
			i.errs.Add(ctx, 1, attr)
		}
		return resp, err
	})
}

func (i *metricsInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *metricsInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return connect.StreamingHandlerFunc(func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		attr := metric.WithAttributes(attribute.String("procedure", conn.Spec().Procedure))
		if i.calls != nil {
			i.calls.Add(ctx, 1, attr)
		}
		err := next(ctx, conn)
		if i.duration != nil {
			i.duration.Record(ctx, time.Since(start).Seconds(), attr)
		}
		if err != nil && i.errs != nil {
			i.errs.Add(ctx, 1, attr)
		}
		return err
	})
}

// RecoveryInterceptor returns a Connect interceptor that recovers from
// panics inside downstream handlers, logs the recovered value plus the
// stack, and returns connect.CodeInternal so the client never sees a
// torn connection.
//
// Place this FIRST in the chain so it observes panics from every
// subsequent interceptor and the handler itself.
func RecoveryInterceptor(logger *slog.Logger) connect.Interceptor {
	if logger == nil {
		logger = slog.Default()
	}
	return &recoveryInterceptor{logger: logger}
}

type recoveryInterceptor struct {
	logger *slog.Logger
}

func (i *recoveryInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (resp connect.AnyResponse, err error) {
		defer func() {
			if r := recover(); r != nil {
				i.logger.ErrorContext(ctx, "panic recovered",
					"procedure", req.Spec().Procedure,
					"panic", r,
					"stack", string(debug.Stack()),
				)
				err = connect.NewError(connect.CodeInternal, panicError(r))
				resp = nil
			}
		}()
		return next(ctx, req)
	})
}

func (i *recoveryInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *recoveryInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return connect.StreamingHandlerFunc(func(ctx context.Context, conn connect.StreamingHandlerConn) (err error) {
		defer func() {
			if r := recover(); r != nil {
				i.logger.ErrorContext(ctx, "panic recovered in stream",
					"procedure", conn.Spec().Procedure,
					"panic", r,
					"stack", string(debug.Stack()),
				)
				err = connect.NewError(connect.CodeInternal, panicError(r))
			}
		}()
		return next(ctx, conn)
	})
}

// panicError wraps a recovered value as an error, preserving the
// original error chain (errors.Is / errors.As work) when the panic
// value is itself an error.
func panicError(r any) error {
	if rerr, ok := r.(error); ok {
		return fmt.Errorf("panic: %w", rerr)
	}
	return fmt.Errorf("panic: %v", r)
}

// RequestIDInterceptor returns a Connect interceptor that ensures every
// inbound request has a correlation ID:
//
//   - If the inbound request carries a non-empty RequestIDHeader, that
//     value is trusted and propagated. This lets edge proxies and
//     upstream services stitch a single trace across hops.
//   - Otherwise a fresh 16-byte hex token is minted via crypto/rand.
//
// The chosen ID is stored on ctx (RequestIDFromContext) and echoed onto
// the response header so clients can log it for later correlation.
//
// Place this AFTER RecoveryInterceptor (so panics still get the ID in
// their log line) and BEFORE LoggingInterceptor (so log records inherit
// the ID).
func RequestIDInterceptor() connect.Interceptor {
	return &requestIDInterceptor{}
}

type requestIDInterceptor struct{}

func (i *requestIDInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		id := req.Header().Get(RequestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		ctx = ContextWithRequestID(ctx, id)
		resp, err := next(ctx, req)
		// Guard against typed-nil: connect handlers that return an error
		// typically also return a typed `*Response[T](nil)` boxed in the
		// AnyResponse interface, so `resp != nil` is true while the
		// underlying pointer is nil and Header() panics. Skip the header
		// write whenever next() returned an error — connect drops the
		// response body in that case so the missing request-id echo is
		// observationally invisible to the client.
		if err == nil && resp != nil {
			resp.Header().Set(RequestIDHeader, id)
		}
		return resp, err
	})
}

func (i *requestIDInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *requestIDInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return connect.StreamingHandlerFunc(func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		id := conn.RequestHeader().Get(RequestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		ctx = ContextWithRequestID(ctx, id)
		conn.ResponseHeader().Set(RequestIDHeader, id)
		return next(ctx, conn)
	})
}

// newRequestID generates a 16-byte random hex string. Avoids the heavier
// ULID dep in pkg/observe (the scaffolded HTTP middleware uses ULID;
// the interceptor only needs a unique-per-request token).
func newRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failure is exceptional; fall back to a
		// monotonically-distinguishable token rather than panicking.
		return fmt.Sprintf("rid-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// noopInterceptor is the pass-through used when a tracer or meter is
// nil. Returning a real interceptor (rather than nil) keeps the
// DefaultMiddlewares chain a fixed length, so callers can index into it
// or rely on its position-stable order.
type noopInterceptor struct{}

func (n *noopInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc { return next }
func (n *noopInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (n *noopInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
