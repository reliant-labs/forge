package observe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/reliant-labs/forge/pkg/svcerr"
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
			attrs = append(attrs, errorAttrs(err)...)
			i.logger.LogAttrs(ctx, LevelForError(err), "rpc failed", attrs...)
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
			attrs = append(attrs, errorAttrs(err)...)
			i.logger.LogAttrs(ctx, LevelForError(err), "stream failed", attrs...)
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

// errorAttrs renders a failed RPC's error for the LOG, which is not the
// same thing as rendering it for the client.
//
// `error` is what the caller was told. `cause` is what actually happened:
// svcerr redacts the message of an unrecognised internal failure before it
// reaches the wire, so without this attribute the driver text — the SQLSTATE,
// the constraint, the panic value — would exist in exactly no place. The
// generated ORM records it on the active span too, but OTEL_EXPORTER_OTLP_
// ENDPOINT is empty by default, so that span goes nowhere in the default
// configuration and cannot be the only copy.
//
// SANITIZE THE WIRE, NEVER THE LOG.
func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{slog.Any("error", err)}
	if cause := svcerr.Cause(err); cause != nil {
		attrs = append(attrs, slog.String("cause", cause.Error()))
	}
	return attrs
}

// LevelForError is the single place that decides how loud a failed RPC is.
//
// Every failure used to log at WARN, including the ones that mean the
// SERVER is broken. A total database outage produced a stream of WARN
// records and a 500 for every request, so the standard alert rule —
// level=ERROR — stayed silent through the whole incident. Meanwhile a
// client sending a malformed field also logged WARN, which is why raising
// everything to ERROR is not the fix either: it just moves the noise.
//
// The split is fault attribution, not HTTP status:
//
//   - ERROR: the server failed and someone should be paged. Internal (a
//     bug or a dependency down), Unavailable (a dependency refused),
//     DataLoss, and Unknown (an error nothing classified — which in
//     practice is a bug in the classification).
//   - WARN: the request failed for a reason the server correctly
//     detected. NotFound, InvalidArgument, PermissionDenied,
//     ResourceExhausted, cancellations. These are the API working.
//
// Exported so the audit interceptor and any project-owned logging site
// classify identically — two log streams disagreeing about severity for
// the same RPC is its own incident.
func LevelForError(err error) slog.Level {
	if err == nil {
		return slog.LevelInfo
	}
	switch connect.CodeOf(err) {
	case connect.CodeInternal, connect.CodeUnknown, connect.CodeDataLoss, connect.CodeUnavailable:
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
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
//
// The result is REDACTED: a panic value is the most internal thing a
// process has — "panic: assignment to entry in nil map", a nil-pointer
// dereference naming an unexported field, or an error carrying whatever
// the failing library put in it — and connect.Error.Message() is
// verbatim err.Error(), so returning it raw published the crash detail
// to whoever triggered it. The client gets svcerr.InternalMessage; the
// panic value and its stack are already logged by the caller, and
// svcerr.Cause / errors.As still reach the original server-side.
func panicError(r any) error {
	var inner error
	if rerr, ok := r.(error); ok {
		inner = fmt.Errorf("panic: %w", rerr)
	} else {
		inner = fmt.Errorf("panic: %v", r)
	}
	return svcerr.WithCause(errors.New(svcerr.InternalMessage), inner)
}

// RequestIDInterceptor returns a Connect interceptor that ensures every
// inbound request has a correlation ID. It resolves the ID in this
// order, and the order is the whole point:
//
//  1. An ID ALREADY ON THE CONTEXT wins. The HTTP edge
//     (pkg/middleware.RequestIDMiddleware) sits outside this interceptor,
//     and it has already picked the ID, written it to the RESPONSE
//     header, and put it on ctx. It does not write it back onto the
//     INBOUND request header, so an interceptor that only consulted
//     req.Header() found nothing and minted a SECOND id — the response
//     came back with two X-Request-Id values, every standard client read
//     the first, and only the second ever reached the logs. Quoting an
//     ID at support and grepping for it returned nothing.
//  2. Otherwise the inbound RequestIDHeader, so an edge proxy or calling
//     service can stitch one ID across hops.
//  3. Otherwise a fresh 16-byte crypto/rand hex token.
//
// ECHO OWNERSHIP follows from the same rule: whichever layer CHOSE the
// ID echoes it. When the ID came off the context, an outer layer chose
// it and this interceptor writes no response header at all — that is
// what keeps exactly one X-Request-Id on the wire. When this interceptor
// chose the ID (no HTTP middleware in the stack, e.g. a bare Connect
// mux) it echoes, so the client is never left without one.
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
		// An outer layer already chose and echoed an ID: adopt it and stay
		// out of the response headers.
		if RequestIDFromContext(ctx) != "" {
			return next(ctx, req)
		}
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
		if RequestIDFromContext(ctx) != "" {
			return next(ctx, conn)
		}
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
