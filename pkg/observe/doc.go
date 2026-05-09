// Package observe provides observability primitives for forge-generated
// services: Connect interceptors for logging, tracing, metrics, recovery,
// and request-id correlation, plus opt-in helpers (LogCall, TraceCall,
// RecordCall) for explicit per-method instrumentation inside internal
// packages.
//
// # Why interceptors, not method-by-method codegen
//
// Earlier forge versions emitted per-package middleware_gen.go,
// tracing_gen.go and metrics_gen.go wrappers around every contract.go
// interface. That covered the case where one internal Service called
// another and you wanted observability at the inner call boundary —
// but the cost was four generated files per internal package, plus a
// regeneration step every time a contract changed.
//
// In practice almost every observability need is a request-scoped one:
// "log this RPC", "trace this RPC", "count this RPC". Connect
// interceptors capture all of those at the handler boundary, once,
// without per-package codegen. Internal-package observability — when
// one Service calls another and you want a child span — is expressed
// opt-in via the Trace/Log/Record helpers: explicit, greppable, and
// only paid when the user actually wants it.
//
// # The interceptor chain
//
// Most projects want a canonical chain. DefaultMiddlewares returns it:
//
//	interceptors := observe.DefaultMiddlewares(observe.DefaultMiddlewareDeps{
//	    Logger:  logger,
//	    Tracer:  tracer,
//	    Meter:   meter,
//	})
//
// Order matters; see the DefaultMiddlewares docstring for the rationale.
//
// Projects that want a custom chain compose interceptors directly:
//
//	interceptors := []connect.Interceptor{
//	    observe.RecoveryInterceptor(logger),
//	    observe.RequestIDInterceptor(),
//	    observe.LoggingInterceptor(logger),
//	    auth.Interceptor(...),
//	}
//
// # Per-method opt-in inside internal packages
//
// When one Service method calls another, wrap the inner call with a
// helper to produce a child span / log / metric:
//
//	func (s *svc) DoThing(ctx context.Context, req Req) (Resp, error) {
//	    return observe.TraceCall(ctx, tracer, "userstore.Get", func(ctx context.Context) (User, error) {
//	        return s.userStore.Get(ctx, req.UserID)
//	    })
//	}
//
// This replaces the auto-generated per-method wrapper with an explicit
// call site. The mock_gen.go file is still emitted by forge generate
// (greppable test seam) — only the middleware/tracing/metrics codegen
// is gone.
package observe
