package observe

import (
	"log/slog"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// DefaultMiddlewareDeps is the dependency bag for DefaultMiddlewares.
// Every field is optional — a nil Logger, Tracer, or Meter cleanly
// degrades the corresponding interceptor (slog.Default for the logger,
// pass-through interceptors for tracer/meter), so projects that haven't
// configured OTel can still use the helper.
type DefaultMiddlewareDeps struct {
	// Logger is the slog.Logger used by RecoveryInterceptor and
	// LoggingInterceptor. nil falls back to slog.Default.
	Logger *slog.Logger

	// Tracer feeds TracingInterceptor. nil disables tracing.
	Tracer trace.Tracer

	// Meter feeds MetricsInterceptor. nil disables metrics.
	Meter metric.Meter

	// Extras are appended to the canonical chain in the order supplied
	// — useful for project-specific interceptors (auth, tenant,
	// rate-limit, idempotency, audit) that the canonical chain doesn't
	// know about. Order is preserved relative to each other and they
	// run AFTER the observability layer.
	Extras []connect.Interceptor
}

// DefaultMiddlewares returns the canonical Connect interceptor chain
// for forge-generated services.
//
// # Order, and why
//
// The order is:
//
//  1. RecoveryInterceptor   — outermost; observes panics from every
//     subsequent layer + the handler. If you put it later, an
//     interceptor crash propagates to the client as a torn connection
//     instead of a clean Internal error.
//
//  2. RequestIDInterceptor — runs early so log records, traces, and
//     metrics from later layers can attribute themselves to the same
//     request ID. Trusts an inbound RequestIDHeader when present,
//     mints a fresh ID otherwise.
//
//  3. LoggingInterceptor   — emits one record per RPC. Sits before
//     tracing/metrics so its timing reflects ALL inner cost (including
//     the OTel work itself). Logging is cheap; the placement is about
//     "this is what the user paid".
//
//  4. TracingInterceptor   — wraps the handler in an OTel span. Inner
//     to logging so the span name is stable per procedure even when
//     auth/tenant rewrites context.
//
//  5. MetricsInterceptor   — innermost observability layer. Records
//     calls/errors/duration. Sitting after tracing means the duration
//     histogram measures handler-only time (excluding upstream
//     observability cost), which is the more meaningful number.
//
//  6. Extras              — project-specific interceptors. The canonical
//     position for auth, tenant, rate-limit, idempotency is INNER to
//     observability (so failures from those layers still get logged,
//     traced and counted) and OUTER to the handler. DefaultMiddlewares
//     appends Extras in the order supplied; callers control inter-Extra
//     ordering.
//
// # Auth-first vs auth-last
//
// A reasonable alternative is "auth at position 1, before everything
// else" — the case being that observability of unauthenticated traffic
// is noise. The forge default is auth-after-observability for two
// reasons:
//
//  1. Operators want to see authentication failures (count, rate, source)
//     in the same dashboards they see successful traffic. Logging and
//     metrics need to run.
//  2. The Connect handler's procedure routing (which the
//     observability layer attributes against) happens BEFORE any
//     interceptor, so observability sees procedure regardless of order.
//
// Projects that disagree can build a custom chain — DefaultMiddlewares
// is opinionated, not mandatory.
//
// # nil deps
//
// nil tracer / nil meter degrade to pass-through interceptors. nil
// logger falls back to slog.Default. This makes DefaultMiddlewares
// safe to wire from a test harness that doesn't configure OTel.
func DefaultMiddlewares(deps DefaultMiddlewareDeps) []connect.Interceptor {
	chain := []connect.Interceptor{
		RecoveryInterceptor(deps.Logger),
		RequestIDInterceptor(),
		LoggingInterceptor(deps.Logger),
		TracingInterceptor(deps.Tracer),
		MetricsInterceptor(deps.Meter),
	}
	chain = append(chain, deps.Extras...)
	return chain
}
