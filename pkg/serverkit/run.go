package serverkit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/reliant-labs/forge/pkg/observe"
)

// Run starts the server and blocks until SIGINT/SIGTERM or a fatal
// error. It owns the full lifecycle described in the package doc:
// logger init, OTel setup, optional auto-migrate, Connect interceptor
// chain, bootstrap, post-bootstrap, health probes, REST handler swap,
// CORS/security/request-id/h2c wrapping, listener bind, worker
// supervision, operator manager gating, and graceful shutdown.
//
// args is the trailing positional slice from the cobra command — when
// non-empty it filters which services bootstrap and which workers run
// (the project's BootstrapOnly path). args is passed through to the
// Bootstrap hook verbatim.
//
// Run is the only entry point projects call. Every other type and
// helper exists to shape the hook surface or document the lifecycle.
func Run(ctx context.Context, cfg Config, hooks Hooks, args []string) error {
	cfg.defaults()

	if hooks.Bootstrap == nil {
		return fmt.Errorf("serverkit.Run: Hooks.Bootstrap is required")
	}
	if cfg.Addr == "" {
		return fmt.Errorf("serverkit.Run: Config.Addr is required")
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	// OTel is optional — degrade gracefully if not configured.
	var (
		shutdownOTel   func(context.Context) error
		metricsHandler http.Handler
	)
	if hooks.SetupOTel != nil {
		s, m, err := hooks.SetupOTel(ctx)
		if err != nil {
			logger.Error("failed to initialize OpenTelemetry", "error", err)
			// Continue without telemetry — this is intentional.
		}
		shutdownOTel = s
		metricsHandler = m
	}

	// Warn loudly when running in development mode. Development mode enables
	// permissive defaults (e.g. authz allow-all) for local ergonomics — this
	// must never be used in production. The permissive behavior is selected
	// at Bootstrap time based on cfg.Environment.
	if cfg.Environment == "development" {
		logger.Warn("running in development mode — permissive authz defaults are enabled. NEVER set ENVIRONMENT=development in production.")
	}

	// Auto-migrate: serverkit dials the DB, applies pool tuning, calls
	// the hook, and closes. The hook owns the actual migration body so
	// projects can pick goose/atlas/their own SQL.
	if cfg.AutoMigrate {
		if hooks.AutoMigrate == nil {
			return fmt.Errorf("serverkit.Run: Config.AutoMigrate is true but Hooks.AutoMigrate is nil")
		}
		if cfg.DatabaseURL == "" {
			return fmt.Errorf("auto-migrate is enabled but DatabaseURL is not set")
		}
		db, dbErr := sql.Open(cfg.DBDriver, cfg.DatabaseURL)
		if dbErr != nil {
			return fmt.Errorf("failed to connect to database for migration: %w", dbErr)
		}
		ApplyDBPoolTuning(db, cfg.DBPoolTuning)
		if migrateErr := hooks.AutoMigrate(ctx, db, logger); migrateErr != nil {
			_ = db.Close()
			return fmt.Errorf("auto-migration failed: %w", migrateErr)
		}
		_ = db.Close()
		logger.Info("db migration completed")
	}

	mux := http.NewServeMux()

	// Expose Prometheus metrics endpoint.
	if metricsHandler != nil {
		mux.Handle("/metrics", metricsHandler)
	}

	// Project-specific interceptors are layered AFTER the canonical
	// observability chain so failures from auth/audit are still
	// observable. The project owns inter-extra ordering (typical chain:
	// otelconnect → rate-limit → auth → audit).
	var projectInterceptors []connect.Interceptor
	if hooks.ProjectInterceptors != nil {
		projectInterceptors = hooks.ProjectInterceptors(logger)
	}

	// observe.DefaultMiddlewares returns the canonical observability
	// chain (recovery → request-id → logging → tracing → metrics).
	// Pass project-specific interceptors via Extras; they run AFTER
	// the canonical chain so auth/audit failures stay observable.
	interceptors := observe.DefaultMiddlewares(observe.DefaultMiddlewareDeps{
		Logger: logger,
		Extras: projectInterceptors,
	})

	opts := []connect.HandlerOption{
		connect.WithInterceptors(interceptors...),
		connect.WithReadMaxBytes(cfg.ReadMaxBytes),
		connect.WithSendMaxBytes(cfg.SendMaxBytes),
	}

	// Bootstrap packages and services. Readiness flips AFTER the
	// listener successfully binds (see ln/Serve below) so /readyz is
	// an accurate signal to load balancers and probes.
	var ready atomic.Bool
	application, bootstrapErr := hooks.Bootstrap(ctx, mux, logger, args, opts...)
	if bootstrapErr != nil {
		return fmt.Errorf("bootstrap failed: %w", bootstrapErr)
	}
	if application == nil {
		return fmt.Errorf("serverkit.Run: Bootstrap returned nil Application")
	}

	// PostBootstrap is the single forge-blessed chokepoint for wiring
	// that depends on a CONSTRUCTED component — wire_gen only resolves
	// Deps fields, so post-construct registrations must happen here.
	if hooks.PostBootstrap != nil {
		if hookErr := hooks.PostBootstrap(application); hookErr != nil {
			return fmt.Errorf("post-bootstrap hook failed: %w", hookErr)
		}
	}

	// Liveness probe: always 200 if the process is alive.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	// Readiness probe: 200 only after bootstrap + listener bind.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	// When `api.rest: true` in forge.yaml, Bootstrap installs a vanguard
	// REST transcoder over the Connect mux and stores the wrapped handler
	// on the Application. Substituting it here means REST/gRPC/Connect
	// share the same handler chain (CORS, security headers, request-id)
	// without the caller having to know which protocol skins are active.
	var handler http.Handler = mux
	if rest := application.RESTHandler(); rest != nil {
		handler = rest
	}

	// CORS: project-supplied middleware factory keeps the existing
	// pkg/middleware.CORSMiddleware in charge of the actual matching
	// logic. serverkit only owns the gating.
	if len(cfg.CORSOrigins) > 0 && hooks.CORSMiddleware != nil {
		handler = hooks.CORSMiddleware(cfg.CORSOrigins, cfg.CORSAllowCredentials)(handler)
	}

	// Security headers — OWASP defaults via project middleware. HSTS is
	// only emitted in production (Environment != "development").
	if cfg.SecurityHeaders && hooks.SecurityHeadersMiddleware != nil {
		production := cfg.Environment != "development"
		handler = hooks.SecurityHeadersMiddleware(production)(handler)
	}

	// RequestID runs at the outermost layer so every subsequent
	// middleware (CORS, security headers, logging) can see the
	// correlation ID on both inbound context and outbound response
	// header.
	if hooks.RequestIDMiddleware != nil {
		handler = hooks.RequestIDMiddleware()(handler)
	}

	// Project-owned outer wrapper. Runs OUTSIDE serverkit's own stack,
	// INSIDE h2c.
	if hooks.HTTPMiddleware != nil {
		handler = hooks.HTTPMiddleware(handler)
	}

	handler = h2c.NewHandler(handler, &http2.Server{})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Cap request header size. Go's default is 1 MiB; we set it
		// explicitly so the limit is obvious and easy to tune.
		MaxHeaderBytes: 1 << 20,
	}

	// Graceful shutdown: serverkit owns the signal context. A caller-
	// supplied ctx is used only for the bootstrap phase; once we hit
	// the serve loop the signal-derived ctx takes over.
	runCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, err := (&net.ListenConfig{}).Listen(runCtx, "tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}

	// The listener is bound and accepting connections; flip readiness
	// so /readyz starts returning 200. Serve below hands off to the
	// accept loop without a second bind step.
	ready.Store(true)

	tls := cfg.TLSCertPath != "" && cfg.TLSKeyPath != ""
	logger.Info("server listening", "addr", cfg.Addr, "tls", tls)

	errCh := make(chan error, 1)
	go func() {
		var serveErr error
		if tls {
			serveErr = srv.ServeTLS(ln, cfg.TLSCertPath, cfg.TLSKeyPath)
		} else {
			serveErr = srv.Serve(ln)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	}()

	// Optional pprof endpoint on a separate listener. pprof must never
	// be mounted on the public handler — it exposes heap/goroutine/
	// profile endpoints that can leak memory contents and stall a
	// running process.
	var pprofSrv *http.Server
	if cfg.PprofAddr != "" {
		pprofMux := http.NewServeMux()
		pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
		pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		pprofSrv = &http.Server{
			Addr:              cfg.PprofAddr,
			Handler:           pprofMux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		pprofLn, lnErr := (&net.ListenConfig{}).Listen(runCtx, "tcp", cfg.PprofAddr)
		if lnErr != nil {
			return fmt.Errorf("listen pprof %s: %w", cfg.PprofAddr, lnErr)
		}
		go func() {
			logger.Info("pprof server starting", "addr", cfg.PprofAddr)
			if serveErr := pprofSrv.Serve(pprofLn); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				logger.Error("pprof server error", "error", serveErr)
			}
		}()
	}

	// Build the set of requested names for filtering (empty = run all).
	nameSet := make(map[string]bool, len(args))
	for _, n := range args {
		nameSet[n] = true
	}

	// Start workers in background goroutines. Each worker gets its own
	// context derived from the shared supervisor context: cancelling
	// workerCtx on shutdown fans out to every per-worker ctx, and one
	// worker exiting early can't disturb its siblings.
	//
	// Workers that implement the optional ContextWorker interface get
	// RunContext (the ctx-aware lifecycle, preferred); everything else
	// falls back to the legacy Start signature unchanged. See the
	// ContextWorker doc for the full shutdown contract.
	workerCtx, workerCancel := context.WithCancel(runCtx)
	defer workerCancel()
	var workerWg sync.WaitGroup

	for _, w := range application.WorkerList() {
		// If a name filter was provided, only start matching workers.
		if len(nameSet) > 0 && !nameSet[w.Name()] {
			continue
		}
		workerWg.Add(1)
		go func(w Worker) {
			defer workerWg.Done()
			wctx, wcancel := context.WithCancel(workerCtx)
			defer wcancel()
			if cw, ok := w.(ContextWorker); ok {
				logger.Info("worker starting", "worker", w.Name(), "lifecycle", "run-context")
				if workErr := cw.RunContext(wctx); workErr != nil && !errors.Is(workErr, context.Canceled) {
					logger.Error("worker error", "worker", w.Name(), "error", workErr)
				}
				return
			}
			logger.Info("worker starting", "worker", w.Name())
			if startErr := w.Start(wctx); startErr != nil {
				logger.Error("worker error", "worker", w.Name(), "error", startErr)
			}
		}(w)
	}

	// Start controller manager for operators (if any).
	//
	// Operator gating: when the user filtered with `server [services...]`
	// args, only start the controller manager if the filter includes at
	// least one operator-shape service. Otherwise an admin-server-only
	// host run logs "controller manager failed: not running in-cluster"
	// because controller-runtime mandates a kubeconfig the host process
	// doesn't have. The unfiltered case (no args) keeps the legacy
	// behaviour — start every operator the project declared.
	if application.HasOperators() && shouldRunOperators(application, nameSet) {
		go func() {
			if runErr := application.RunOperators(runCtx, logger, cfg.OperatorHealthProbeAddr); runErr != nil {
				logger.Error("controller manager failed", "error", runErr)
			}
		}()
	}

	var runErr error
	select {
	case err := <-errCh:
		logger.Error("server error", "error", err)
		runErr = fmt.Errorf("server failed: %w", err)
	case <-runCtx.Done():
	}
	logger.Info("server stopping")

	// Graceful shutdown sequence:
	//   1. Flip readiness to false so /readyz starts failing.
	//   2. Sleep pre_stop_delay so load balancers observe the failing
	//      probe and stop routing new traffic to this replica.
	//   3. Begin srv.Shutdown, bounded by shutdown_timeout, so in-flight
	//      requests drain without accepting new ones.
	// Without step 1 + 2, srv.Shutdown stops accepting new conns
	// immediately but the LB keeps sending them until it next polls
	// /readyz — producing brief but real 502s on every rollout.
	ready.Store(false)

	if cfg.PreStopDelay > 0 {
		logger.Info("readiness flipped, waiting for LB drain", "pre_stop_delay", cfg.PreStopDelay)
		time.Sleep(cfg.PreStopDelay)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	// Stop workers first (cancel context + wait + call Stop).
	workerCancel()
	workerWg.Wait()
	for _, w := range application.WorkerList() {
		if len(nameSet) > 0 && !nameSet[w.Name()] {
			continue
		}
		if stopErr := w.Stop(shutdownCtx); stopErr != nil {
			logger.Error("worker stop error", "worker", w.Name(), "error", stopErr)
		}
	}

	if shutErr := application.Shutdown(shutdownCtx); shutErr != nil {
		logger.Error("shutdown error", "error", shutErr)
	}
	if shutErr := srv.Shutdown(shutdownCtx); shutErr != nil {
		logger.Error("server shutdown error", "error", shutErr)
	}
	if pprofSrv != nil {
		if shutErr := pprofSrv.Shutdown(shutdownCtx); shutErr != nil {
			logger.Error("pprof shutdown error", "error", shutErr)
		}
	}
	if shutdownOTel != nil {
		if shutErr := shutdownOTel(shutdownCtx); shutErr != nil {
			logger.Error("otel shutdown error", "error", shutErr)
		}
	}
	return runErr
}

// newLogger builds the slog.Logger Run dispatches on. LOG_FORMAT picks
// between structured JSON (for log aggregators) and human-friendly text
// (for local dev tails); anything other than "text" emits JSON.
func newLogger(cfg Config) *slog.Logger {
	var handler slog.Handler
	switch cfg.LogFormat {
	case "text":
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	default:
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	}
	return slog.New(handler)
}

// shouldRunOperators decides whether the controller manager should
// start given the user's `server [services...]` filter. The unfiltered
// case (nameSet empty) preserves the legacy "always start all
// operators" behaviour. When a filter is active, we only start the
// manager if the filter explicitly includes at least one operator-
// shape service.
func shouldRunOperators(app Application, nameSet map[string]bool) bool {
	if len(nameSet) == 0 {
		return true
	}
	for _, op := range app.OperatorList() {
		if nameSet[op.Name()] {
			return true
		}
	}
	return false
}
