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
//
// # The environment contract
//
// The interface between a forge app and whatever collector exists is the
// standard OpenTelemetry environment. The app never learns the backend: the
// platform renders these variables and Setup obeys them.
//
//	OTEL_EXPORTER_OTLP_ENDPOINT   base URL of the collector, e.g. http://127.0.0.1:4318
//	OTEL_EXPORTER_OTLP_PROTOCOL   http/protobuf (default, port 4318) or grpc (port 4317)
//	OTEL_EXPORTER_OTLP_HEADERS    k=v,k2=v2 — e.g. authorization=<key>
//	OTEL_EXPORTER_OTLP_{TRACES,METRICS,LOGS}_{ENDPOINT,PROTOCOL,HEADERS}
//	                              per-signal overrides of the three above
//	OTEL_SERVICE_NAME             wins over Config.ServiceName (the compile-time default)
//	OTEL_RESOURCE_ATTRIBUTES      merged into the resource; carries
//	                              deployment.environment.name=<forge env name>
//	OTEL_SDK_DISABLED=true        every export off
//	OTEL_{TRACES,METRICS}_EXPORTER=none   that signal off
//	OTEL_LOGS_EXPORTER=otlp       the explicit switch that turns OTLP logs ON
//
// Timeout, compression, TLS and the rest of the OTLP exporter variables are
// read by the exporters themselves. Config.OTLPEndpoint, when set, overrides the
// endpoint variables (it is a base URL; /v1/<signal> is appended for
// http/protobuf); headers and protocol always come from the environment.
// otelenv.go is the only file in this package that calls os.Getenv.
//
// With no endpoint from any source nothing is exported — only the Prometheus
// /metrics reader is wired — but the W3C TraceContext+Baggage propagator is
// installed regardless, so an incoming traceparent is still honoured and
// forwarded by every observe client.
//
// With export on, Setup also records Go runtime metrics
// (go.opentelemetry.io/contrib/instrumentation/runtime).
//
// # Logs
//
// serverkit's logger writes JSON lines to stdout. NewLogHandler adds trace_id
// and span_id to every record logged with a context that holds a valid span
// (use the *Context methods: logger.InfoContext(ctx, ...)). Log shipping is
// ONE path per environment: the collector reads the stdout/file stream. The
// OTLP logs exporter (OTEL_LOGS_EXPORTER=otlp or Config.OTLPLogs) is off by
// default and is MUTUALLY EXCLUSIVE with that collection: it does not replace
// stdout, so enabling both ships every line twice.
package observe
