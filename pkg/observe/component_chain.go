// File: component_chain.go — the IN-PROCESS component middleware chain.
//
// This is the in-process twin of the Connect edge interceptor chain
// (Chain / DefaultMiddlewares in middleware.go). Those interceptors wrap
// every RPC at the transport boundary; a ComponentChain wraps every method
// of an internal component (a contract.go Service) at the in-process call
// boundary — one span / metric / log per method call, without the method's
// body knowing anything about it.
//
// A forge-generated decorator (middleware_gen.go) routes each interface
// method through the chain with a single line:
//
//	func (o *forgeMiddlewareService) Do(ctx context.Context, in DoInput) (DoResult, error) {
//	    return observe.Around(ctx, o.chain, "checkout.Do", func(ctx context.Context) (DoResult, error) {
//	        return o.inner.Do(ctx, in)
//	    })
//	}
//
// The chain itself is assembled in the OWNED per-package seam
// observe_chain.go (newObserveChain) from the middlewares below plus any the
// user adds. The generated decorator is uniform: it never names a middleware
// or bakes a parameter (log level, sampling) — those live in the owned seam
// and in forge.yaml. Middleware selection and configuration is a runtime /
// composition concern, exactly like the Connect chain.
package observe

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ComponentOp is the terminal in-process operation a ComponentChain wraps:
// it runs the inner method call and returns only its error. A method's
// non-error results are captured out of the caller's closure (see Around /
// Run), so a middleware never has to be generic over the return type — it
// observes identity, duration, and error status only, which is all a span,
// metric, or log line needs.
type ComponentOp func(context.Context) error

// ComponentMiddleware wraps one in-process method call. Implementations
// start a span, time the call, recover a panic, emit a log line, etc.,
// then invoke next(ctx) and observe the returned error. method is the
// fully-qualified operation name ("<pkg>.<Method>").
//
// A middleware MUST call next(ctx) exactly once (passing a possibly-
// enriched ctx) and return its error — the chain has no notion of
// short-circuiting a component call.
type ComponentMiddleware interface {
	WrapComponent(ctx context.Context, method string, next ComponentOp) error
}

// ComponentChain is an ordered, reusable stack of ComponentMiddleware shared
// by every method of one component decorator. Build it once in the owned
// observe_chain.go seam (NewComponentChain) and route every method through it
// via Around / Run. The zero value (and a nil *ComponentChain) is a valid
// pass-through: methods still execute, just without instrumentation — so a
// decorator wired in a test harness that skipped chain construction is safe.
//
// mw[0] is the OUTERMOST layer (runs first, sees everything inner); the inner
// method call runs last. This matches the Connect chain's outer-to-inner
// ordering.
type ComponentChain struct {
	mw []ComponentMiddleware
}

// NewComponentChain builds a ComponentChain from mw in outer-to-inner order.
// nil entries are dropped so the owned seam can pass a conditionally-disabled
// middleware straight through (e.g. `enabled ? mw : nil`) without a length
// change, mirroring how Chain treats nil collaborators.
func NewComponentChain(mw ...ComponentMiddleware) *ComponentChain {
	kept := make([]ComponentMiddleware, 0, len(mw))
	for _, m := range mw {
		if m != nil {
			kept = append(kept, m)
		}
	}
	return &ComponentChain{mw: kept}
}

// dispatch threads next through middleware i..end, outermost first. Each
// level allocates one closure per call — the same per-call cost the previous
// hand-written decorator paid for its span-start + RecordCall, and cheap
// relative to any method doing real work (DB / adapter / HTTP).
func (c *ComponentChain) dispatch(ctx context.Context, i int, method string, next ComponentOp) error {
	if c == nil || i >= len(c.mw) {
		return next(ctx)
	}
	return c.mw[i].WrapComponent(ctx, method, func(ctx context.Context) error {
		return c.dispatch(ctx, i+1, method, next)
	})
}

// Run routes an error-only (or result-captured) operation through chain and
// returns its error. It is the general primitive: the generated decorator
// uses it directly for error-only methods and — capturing results in the
// closure — for methods with multiple non-error results. A nil chain runs
// next directly.
func Run(ctx context.Context, chain *ComponentChain, method string, next ComponentOp) error {
	return chain.dispatch(ctx, 0, method, next)
}

// Around routes a value-returning method through chain. T is the method's
// single non-error result; the value is captured out of the closure so the
// chain only ever sees the error. It keeps the generated per-method body a
// one-liner for the common (T, error) shape.
func Around[T any](ctx context.Context, chain *ComponentChain, method string, next func(context.Context) (T, error)) (T, error) {
	var out T
	err := chain.dispatch(ctx, 0, method, func(ctx context.Context) error {
		var e error
		out, e = next(ctx)
		return e
	})
	return out, err
}

// ─── Standard middlewares ───────────────────────────────────────────────
//
// Each is a small value type constructed by the owned seam. All degrade
// cleanly on a nil dependency (nil tracer/meter → pass-through, nil logger →
// slog.Default), so a decorator wired without a configured OTel SDK still
// works — the same nil-tolerance the Connect interceptors and the LogCall /
// TraceCall / NewCallMetrics helpers already have.

// RecoverMiddleware converts a panic in the inner call (or any inner
// middleware) into an error, logs it with a stack trace, and returns it — so
// one component's panic can't tear down the caller. It is the OUTERMOST layer
// in the canonical seam so it observes panics from every inner middleware too.
func RecoverMiddleware(logger *slog.Logger) ComponentMiddleware {
	return recoverMiddleware{logger: logger}
}

type recoverMiddleware struct{ logger *slog.Logger }

func (m recoverMiddleware) WrapComponent(ctx context.Context, method string, next ComponentOp) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logger := m.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.LogAttrs(ctx, slog.LevelError, method+": panic recovered",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
			)
			err = fmt.Errorf("panic in %s: %v", method, r)
		}
	}()
	return next(ctx)
}

// TraceMiddleware opens one OTel span per call named method, threads the
// span's context into the inner call, and records the returned error onto the
// span. nil tracer disables tracing (pass-through) — same degradation as
// TraceCall.
func TraceMiddleware(tracer trace.Tracer) ComponentMiddleware {
	return traceMiddleware{tracer: tracer}
}

type traceMiddleware struct{ tracer trace.Tracer }

func (m traceMiddleware) WrapComponent(ctx context.Context, method string, next ComponentOp) error {
	if m.tracer == nil {
		return next(ctx)
	}
	ctx, span := m.tracer.Start(ctx, method)
	defer span.End()
	err := next(ctx)
	RecordSpanError(span, err)
	return err
}

// MetricsMiddleware records call / error / duration metrics under the
// "<namespace>.{calls,errors,duration}" instruments (see NewCallMetrics),
// tagging each with the method attribute set to the operation name. nil meter
// yields a no-op middleware, so a chain built without a configured OTel SDK
// is safe.
func MetricsMiddleware(meter metric.Meter, namespace string) ComponentMiddleware {
	return metricsMiddleware{metrics: NewCallMetrics(meter, namespace)}
}

type metricsMiddleware struct{ metrics *CallMetrics }

func (m metricsMiddleware) WrapComponent(ctx context.Context, method string, next ComponentOp) error {
	start := time.Now()
	err := next(ctx)
	m.metrics.RecordCall(ctx, method, start, err)
	return err
}

// LogMiddleware emits one structured log record per call: the method name as
// the message, the duration as an attribute, and — on failure — the error at
// slog.LevelError. Successful calls log at level, which the owned seam wires
// from forge.yaml's observability.log_level (default Debug: quiet on success
// under a production Info handler, loud on error). nil logger falls back to
// slog.Default.
func LogMiddleware(logger *slog.Logger, level slog.Level) ComponentMiddleware {
	return logMiddleware{logger: logger, level: level}
}

type logMiddleware struct {
	logger *slog.Logger
	level  slog.Level
}

func (m logMiddleware) WrapComponent(ctx context.Context, method string, next ComponentOp) error {
	start := time.Now()
	err := next(ctx)
	logger := m.logger
	if logger == nil {
		logger = slog.Default()
	}
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelError, method,
			slog.Duration("duration", time.Since(start)),
			slog.Any("error", err),
		)
		return err
	}
	logger.LogAttrs(ctx, m.level, method,
		slog.Duration("duration", time.Since(start)),
	)
	return err
}
