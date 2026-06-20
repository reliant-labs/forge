// Package serverkit runs an already-composed forge server: HTTP/Connect
// listener, healthz/readyz probes, the HTTP edge (CORS / security-headers
// / request-id / h2c), worker supervisor, operator manager, and graceful
// shutdown sequence.
//
// # Why a library
//
// Earlier forge versions emitted a ~520-line cmd/server.go per project,
// 95% of which was uniform across every project (signal handling,
// listener bind, readiness flip, worker fan-out, shutdown ordering).
// serverkit absorbs those uniform sections so they get one test suite and
// can evolve without touching every downstream project's checkout.
//
// serverkit takes COMPOSED INPUTS and owns only the uniform lifecycle. It
// knows nothing about service SELECTION: which services are mounted, which
// workers/operators run, and how the interceptor chain / REST swap / OTel
// /metrics handler were built all happen ABOVE serverkit, in the generated
// cmd-server shim. serverkit receives the finished http.Handler plus the
// already-selected Workers/Operators and an OnShutdown closure.
//
// # Usage in generated code
//
// The generated cmd/server.go composes the server and hands it to Run:
//
//	mux := http.NewServeMux()
//	shutdownOTel, metricsHandler, _ := setupOTel(ctx)
//	if metricsHandler != nil { mux.Handle("/metrics", metricsHandler) }
//	// run migrations (the cmd opens the DB it needs)
//	opts := connectHandlerOptions(logger) // observe.DefaultMiddlewares + project interceptors
//	// FORGE_SHAPE_REDESIGN §2 hybrid DI: owned infra → generated injector →
//	// owned two-phase wiring, then per-subcommand mount selection over the
//	// data-only inventory.
//	infra, _ := app.OpenInfra(ctx, cfg, logger)
//	services, _ := app.Build(infra)
//	_ = app.PostBuild(services)
//	mounted := mountServices(services, mux, cfg, logger, names, opts...) // app.Inventory rows
//	var handler http.Handler = mux
//	if rest := restHandler(mux, mounted); rest != nil { handler = rest }
//	return serverkit.Run(ctx, projectConfig(cfg), serverkit.Server{
//	    Handler:    handler,
//	    Logger:     logger,
//	    Workers:    selected(app.WorkerList(services), names),
//	    Operators:  selected(app.OperatorList(services), names),
//	    RunOperators: func(ctx context.Context, l *slog.Logger, addr string) error {
//	        return app.RunOperators(services, ctx, l, addr)
//	    },
//	    OnShutdown: func(ctx context.Context) error { return shutdownOTel(ctx) },
//	    CORSMiddleware:            fmw.CORSMiddleware,
//	    SecurityHeadersMiddleware: securityHeaders,
//	    RequestIDMiddleware:       fmw.RequestIDMiddleware,
//	})
//
// # The Server value
//
// Server carries everything serverkit runs: the composed Handler, the
// selected Workers/Operators, the RunOperators manager entry point, an
// OnShutdown teardown closure, and the project's edge-middleware factories
// (their concrete implementations live in the project's pkg/middleware, so
// they stay fields rather than collapsing to Config). Config still owns the
// GATING for the edge layers (CORSOrigins, SecurityHeaders, Environment).
//
// # Lifecycle
//
// Run owns the uniform lifecycle:
//
//  1. Logger init (Server.Logger, or built from Config when nil).
//  2. A tiny top mux routes /healthz + /readyz to serverkit's own probes
//     (IN FRONT of the edge wrap, so probes are never behind CORS/auth)
//     and everything else to Server.Handler.
//  3. CORS, security-headers, request-id, and h2c are layered over that
//     top mux from Config gating + the Server factory fields.
//  4. Listener binds, readiness flips true.
//  5. Workers start; operator manager starts when len(Operators) > 0 and
//     the RUN_OPERATORS gate allows.
//  6. SIGINT/SIGTERM → readiness flips false → pre-stop sleep →
//     workers stop → Server.OnShutdown → http.Server shuts down →
//     pprof shuts down.
//
// # Worker shutdown contract
//
// Each worker runs in its own goroutine with a per-worker context
// derived from the run lifecycle. On SIGINT/SIGTERM that context is
// cancelled immediately — before the pre-stop drain sleep — so
// long-running cycles get the full shutdown window to unwind; on a
// fatal serve error it is cancelled when the worker-stop phase begins.
// Workers implementing the optional ContextWorker interface run via
// RunContext(ctx), the preferred ctx-aware lifecycle; all other workers
// run via the legacy Start(ctx) with the same per-worker context. The
// supervisor waits for every worker goroutine to return, then calls each
// worker's Stop bounded by Config.ShutdownTimeout. See ContextWorker for
// the full contract.
//
// # What does not belong here
//
// Service SELECTION and COMPOSITION stay in the generated cmd-server
// shim: mux build, service mount (via the existing appkit mechanism),
// the interceptor chain, the REST transcoder swap, OTel setup, and
// auto-migration. Per-service DI wiring stays in the generated
// bootstrap.go — that body is genuinely typed and does not compress into
// a library. serverkit holds only the parts that look the same in every
// forge project.
package serverkit
