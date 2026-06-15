// Package serverkit runs a forge-generated Application: HTTP/Connect
// listener, observability chain, healthz/readyz probes, worker
// supervisor, operator manager, and graceful shutdown sequence.
//
// # Why a library
//
// Earlier forge versions emitted a ~520-line cmd/server.go per project,
// 95% of which was uniform across every project (signal handling,
// listener bind, readiness flip, worker fan-out, shutdown ordering).
// Only the bootstrap call site is genuinely project-typed — it returns
// the project's *app.App, whose shape comes from forge.yaml services.
//
// serverkit absorbs the uniform sections behind a small Hooks struct.
// The generated cmd/server.go shrinks to ~50 lines of config projection
// and hook wiring; everything else lives here, gets one test suite, and
// can evolve without touching every downstream project's checkout.
//
// # Usage in generated code
//
// The generated cmd/server.go calls serverkit.Run with a Config
// projected from the project's *config.Config and a Hooks bag holding
// the project-typed callbacks:
//
//	return serverkit.Run(ctx, projectConfig(cfg), serverkit.Hooks{
//	    Bootstrap: func(ctx context.Context, mux *http.ServeMux, logger *slog.Logger, names []string, opts ...connect.HandlerOption) (serverkit.Application, error) {
//	        if len(names) > 0 {
//	            return app.BootstrapOnly(mux, logger, cfg, names, opts...)
//	        }
//	        return app.Bootstrap(mux, logger, cfg, opts...)
//	    },
//	    PostBootstrap: func(a serverkit.Application) error {
//	        return app.PostBootstrap(a.(*app.App))
//	    },
//	    AutoMigrate:         func(ctx context.Context, l *slog.Logger) error { return app.AutoMigrate(ctx, cfg, l) },
//	    SetupOTel:           setupOTel,
//	    ProjectInterceptors: func(l *slog.Logger) []connect.Interceptor { ... },
//	}, args)
//
// # Application interface
//
// serverkit needs only a narrow view of the project's *app.App: the
// Worker and Operator lists, an optional REST handler, and a Shutdown
// hook. The generated *app.App satisfies Application with one-line
// adapter methods; serverkit never imports pkg/app.
//
// # Lifecycle
//
// Run owns the full lifecycle:
//
//  1. Logger init (slog level + json/text), then SetupOTel hook.
//  2. Optional AutoMigrate (serverkit dials the DB, applies pool tuning
//     from Config.DBPoolTuning, calls the hook, closes).
//  3. Connect interceptor chain: observe.DefaultMiddlewares wraps the
//     project-supplied interceptors (auth/audit/rate-limit/otelconnect).
//  4. Bootstrap hook constructs the Application and mounts handlers.
//  5. PostBootstrap hook runs.
//  6. /healthz and /readyz mounted; CORS, security-headers, request-id,
//     and h2c are layered over the handler stack.
//  7. Listener binds, readiness flips true.
//  8. Workers start; operator manager starts when gating allows.
//  9. SIGINT/SIGTERM → readiness flips false → pre-stop sleep →
//     workers stop → application shuts down → http.Server shuts down →
//     pprof shuts down → OTel flushes.
//
// # Worker shutdown contract
//
// Each worker runs in its own goroutine with a per-worker context
// derived from the run lifecycle. On SIGINT/SIGTERM that context is
// cancelled immediately — before the pre-stop drain sleep — so
// long-running cycles get the full shutdown window to unwind; on a
// fatal serve error it is cancelled when the worker-stop phase begins.
// Workers implementing the optional
// ContextWorker interface run via RunContext(ctx), the preferred
// ctx-aware lifecycle; all other workers run via the legacy Start(ctx)
// with the same per-worker context. The supervisor waits for every
// worker goroutine to return, then calls each worker's Stop bounded by
// Config.ShutdownTimeout. See ContextWorker for the full contract.
//
// # What does not belong here
//
// Per-service DI wiring stays in the generated bootstrap.go — that body
// is genuinely typed (Deps shapes, Register calls, AuthzInterceptor per
// service) and does not compress into a library. serverkit holds only
// the parts that look the same in every forge project.
package serverkit
