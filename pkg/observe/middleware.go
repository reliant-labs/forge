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

// Deps is the EXPLICIT-COLLABORATOR dependency bag for [Chain], the
// composition-root variant of [DefaultMiddlewares].
//
// # Why a second bag
//
// [DefaultMiddlewareDeps] models the observability layer plus an opaque
// Extras slice the caller assembles by hand. That worked when the cmd
// shim built the auth / audit / rate-limit interceptors itself and append-
// ordered them into Extras. Under explicit per-server composition the
// composition root has those collaborators as LOCALS (it just constructed
// the token validator, the audit sink interceptor, the rate limiter) and
// wants to hand them in by NAME — not pack them into a positional slice and
// rely on getting the canonical inner-order right. Deps names each project
// concern so a project NEVER reaches for a package-global
// (SetTokenValidator / SetAuditStore / SetIdentityEnricher): it passes the
// already-built interceptor straight into the field.
//
// Every collaborator field is a [connect.Interceptor] (or nil to skip it),
// which is exactly what the project's builders already produce —
// authn.NewInterceptor for Auth, middleware.AuditInterceptor for Audit,
// middleware.RateLimitInterceptor for RateLimit. Passing the constructed
// VALUE (not a setter) is the whole point: the wiring is visible at the
// call site and testable without globals.
type Deps struct {
	// Logger feeds RecoveryInterceptor and LoggingInterceptor. nil falls
	// back to slog.Default — same degradation as DefaultMiddlewareDeps.
	Logger *slog.Logger

	// Tracer feeds TracingInterceptor. nil disables tracing.
	Tracer trace.Tracer

	// Meter feeds MetricsInterceptor. nil disables metrics.
	Meter metric.Meter

	// Auth is the project's authentication interceptor — the value
	// returned by authn.NewInterceptor(policy), with the token validator
	// and identity enricher already threaded through the Policy. This is
	// the field that retires middleware.SetTokenValidator /
	// SetIdentityEnricher: the composition root builds the interceptor
	// with its validator in hand and passes it here. nil skips the auth
	// layer entirely (e.g. a worker-only process, or a test harness).
	Auth connect.Interceptor

	// Audit is the project's audit interceptor — the value returned by
	// middleware.AuditInterceptor(sink, ...), with the durable audit sink
	// already bound. This retires middleware.SetAuditStore: the
	// composition root opens the store, builds the interceptor around it,
	// and passes it here. nil skips audit.
	Audit connect.Interceptor

	// RateLimit is the project's rate-limit interceptor — the value
	// returned by middleware.RateLimitInterceptor(opts, claimsLookup), or
	// nil when rate limiting is disabled (RateLimitInterceptor itself
	// returns nil for Rps <= 0, so the composition root can pass its
	// result through unconditionally and a disabled limiter cleanly drops
	// out of the chain).
	RateLimit connect.Interceptor

	// Extras are additional project-specific interceptors (tenant
	// scoping, idempotency, anything the named fields don't cover),
	// appended AFTER the named application layer in the order supplied.
	// Inter-Extra order is the caller's to control.
	Extras []connect.Interceptor
}

// Chain returns the canonical Connect interceptor chain for a project
// composed with EXPLICIT collaborators — the per-server composition-root
// counterpart to [DefaultMiddlewares].
//
// The order is the canonical forge order, with the application layer
// (auth → audit → rate-limit) sitting INNER to the observability layer and
// OUTER to the handler, exactly the position DefaultMiddlewares documents
// for Extras:
//
//  1. RecoveryInterceptor   — outermost; observes panics from everything.
//  2. RequestIDInterceptor — mints/propagates the correlation id early.
//  3. LoggingInterceptor   — one record per RPC.
//  4. TracingInterceptor   — one OTel span per RPC.
//  5. MetricsInterceptor   — calls/errors/duration.
//  6. Auth                 — authenticate (when non-nil). Inner to
//     observability so auth failures are still logged / traced / counted.
//  7. Audit                — durable audit record (when non-nil); runs
//     after auth so it sees the authenticated principal.
//  8. RateLimit            — throttle (when non-nil); keyed off the
//     authenticated subject auth attached, so it follows auth.
//  9. Extras               — remaining project interceptors, in order.
//
// nil collaborator fields are simply skipped — the chain has no fixed
// length (unlike the observability-only DefaultMiddlewares, which keeps a
// stable length via no-op interceptors). A worker process passing all-nil
// application collaborators gets the pure observability chain.
func Chain(deps Deps) []connect.Interceptor {
	chain := []connect.Interceptor{
		RecoveryInterceptor(deps.Logger),
		RequestIDInterceptor(),
		LoggingInterceptor(deps.Logger),
		TracingInterceptor(deps.Tracer),
		MetricsInterceptor(deps.Meter),
	}
	if deps.Auth != nil {
		chain = append(chain, deps.Auth)
	}
	if deps.Audit != nil {
		chain = append(chain, deps.Audit)
	}
	if deps.RateLimit != nil {
		chain = append(chain, deps.RateLimit)
	}
	chain = append(chain, deps.Extras...)
	return chain
}
