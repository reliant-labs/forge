package serverkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/reliant-labs/forge/pkg/observe"
)

// Run starts the server and blocks until SIGINT/SIGTERM or a fatal
// error. It takes an ALREADY-COMPOSED Server (handler with services
// mounted, selected workers/operators, an OnShutdown closure) and owns
// only the uniform lifecycle: logger init, health probes, the HTTP edge
// (CORS/security/request-id/h2c driven by Config + Server's factory
// fields), listener bind, worker supervision, operator-manager gating,
// graceful shutdown, and pprof.
//
// Service SELECTION (the old args/names filter) and composition (mux
// build, service mount, interceptor chain, OTel setup, auto-migrate) all
// happen ABOVE serverkit in the generated cmd-server shim. serverkit no
// longer receives names and knows nothing about which services run.
//
// Run is the only entry point projects call. Every other type and
// helper exists to shape the Server surface or document the lifecycle.
func Run(ctx context.Context, cfg Config, srv Server) error {
	cfg.defaults()

	if srv.Handler == nil {
		return fmt.Errorf("serverkit.Run: Server.Handler is required")
	}
	if cfg.Addr == "" {
		return fmt.Errorf("serverkit.Run: Config.Addr is required")
	}
	if len(srv.Operators) > 0 && srv.RunOperators == nil {
		return fmt.Errorf("serverkit.Run: Server.Operators is non-empty but Server.RunOperators is nil")
	}

	// Logger: prefer the caller's (so mount-time and run-time logs share
	// attrs); build one from Config only when the caller passed nil.
	logger := srv.Logger
	if logger == nil {
		logger = newLogger(cfg)
	}
	slog.SetDefault(logger)

	// Warn loudly when running in development mode. Development mode enables
	// permissive defaults (e.g. authz allow-all) for local ergonomics — this
	// must never be used in production. The permissive behavior is selected
	// by the caller (at mount time) based on cfg.Environment.
	if cfg.Environment == "development" {
		logger.Warn("running in development mode — permissive authz defaults are enabled. NEVER set ENVIRONMENT=development in production.")
	}

	// OTel: serverkit OWNS OpenTelemetry setup (the generated cmd/otel.go
	// shim is gone). observe.Setup installs the global trace/metric
	// providers from cfg.OTLPEndpoint + cfg.ServiceName, always wires the
	// Prometheus reader, and returns the /metrics handler (mounted on the
	// top mux below, IN FRONT of the edge so scrapers bypass CORS/auth) and
	// a shutdown fn (flushed in the graceful-shutdown sequence). A setup
	// error is logged, not fatal — projects depending on OTLP fail config
	// validation before Run.
	instanceID, _ := os.Hostname()
	otelShutdown, metricsHandler, otelErr := observe.Setup(ctx, observe.Config{
		ServiceName:    cfg.ServiceName,
		ServiceVersion: cfg.ServiceVersion,
		OTLPEndpoint:   cfg.OTLPEndpoint,
		InstanceID:     instanceID,
	})
	if otelErr != nil {
		logger.Error("failed to initialize OpenTelemetry", "error", otelErr)
	}

	// Readiness flips AFTER the listener successfully binds (see ln/Serve
	// below) so /readyz is an accurate signal to load balancers.
	var ready atomic.Bool

	// serverkit no longer owns the service mux, so it can't MOUNT its
	// probes on it. Instead it applies the edge wrap to the CALLER's
	// handler only, then builds a tiny top mux that routes /healthz +
	// /readyz to its own probes and everything else to the edge-wrapped
	// handler. The probes therefore sit IN FRONT of the edge — they are
	// never gated behind CORS, auth, or security-headers middleware.
	var handler http.Handler = srv.Handler

	// CORS: project-supplied middleware factory keeps the existing
	// pkg/middleware.CORSMiddleware in charge of the actual matching
	// logic. serverkit only owns the gating.
	if len(cfg.CORSOrigins) > 0 && srv.CORSMiddleware != nil {
		handler = srv.CORSMiddleware(cfg.CORSOrigins, cfg.CORSAllowCredentials)(handler)
	}

	// Security headers — OWASP defaults via project middleware. HSTS is
	// only emitted in production (Environment != "development").
	if cfg.SecurityHeaders && srv.SecurityHeadersMiddleware != nil {
		production := cfg.Environment != "development"
		handler = srv.SecurityHeadersMiddleware(production)(handler)
	}

	// RequestID runs at the outermost layer so every subsequent
	// middleware (CORS, security headers, logging) can see the
	// correlation ID on both inbound context and outbound response
	// header.
	if srv.RequestIDMiddleware != nil {
		handler = srv.RequestIDMiddleware()(handler)
	}

	// Project-owned outer wrapper. Runs OUTSIDE serverkit's own stack,
	// INSIDE h2c.
	if srv.HTTPMiddleware != nil {
		handler = srv.HTTPMiddleware(handler)
	}

	// Top mux: probes bypass the edge; all other paths hit the
	// edge-wrapped caller handler.
	top := http.NewServeMux()
	top.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	top.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	// /metrics is mounted on the top mux (in front of the edge) so Prometheus
	// scrapers reach it without CORS/auth/security-headers. serverkit owns it
	// now that it owns OTel setup.
	if metricsHandler != nil {
		top.Handle("/metrics", metricsHandler)
	}
	top.Handle("/", handler)

	finalHandler := h2c.NewHandler(top, &http2.Server{})

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           finalHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Cap request header size. Go's default is 1 MiB; we set it
		// explicitly so the limit is obvious and easy to tune.
		MaxHeaderBytes: 1 << 20,
	}

	// Graceful shutdown: serverkit owns the signal context. The caller-
	// supplied ctx governed composition (mount, migrate, OTel setup)
	// before Run was entered; from here the signal-derived ctx drives the
	// serve loop and shutdown.
	runCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// failComponent dispatches a supervised-component failure (worker
	// Start/RunContext error, RunOperators error) on Config.FailurePolicy.
	// FailProcess (the default) records the first failure and cancels
	// runCtx so the normal graceful-shutdown path executes and Run
	// returns the error — the pod restarts loudly instead of serving on
	// with a silently-dead component. Ignore logs and continues.
	var (
		componentMu      sync.Mutex
		componentFailure error
	)
	failComponent := func(component string, err error) {
		if cfg.FailurePolicy == Ignore {
			logger.Error("component failed — continuing (FailurePolicy=Ignore)",
				"component", component, "error", err)
			return
		}
		logger.Error("component failed — terminating process (FailurePolicy=FailProcess)",
			"component", component, "error", err)
		componentMu.Lock()
		if componentFailure == nil {
			componentFailure = fmt.Errorf("%s failed: %w", component, err)
		}
		componentMu.Unlock()
		stop()
	}

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
			serveErr = httpSrv.ServeTLS(ln, cfg.TLSCertPath, cfg.TLSKeyPath)
		} else {
			serveErr = httpSrv.Serve(ln)
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

	// Start workers in background goroutines. Each worker gets its own
	// context derived from the shared supervisor context: cancelling
	// workerCtx on shutdown fans out to every per-worker ctx, and one
	// worker exiting early can't disturb its siblings.
	//
	// Selection already happened above serverkit — srv.Workers holds
	// exactly the workers this process should run, so there's no name
	// filter here. Workers that implement the optional ContextWorker
	// interface get RunContext (the ctx-aware lifecycle, preferred);
	// everything else falls back to the legacy Start signature
	// unchanged. See the ContextWorker doc for the full shutdown
	// contract.
	workerCtx, workerCancel := context.WithCancel(runCtx)
	defer workerCancel()
	var workerWg sync.WaitGroup

	for _, w := range srv.Workers {
		workerWg.Add(1)
		go func(w Worker) {
			defer workerWg.Done()
			wctx, wcancel := context.WithCancel(workerCtx)
			defer wcancel()
			if cw, ok := w.(ContextWorker); ok {
				logger.Info("worker starting", "worker", w.Name(), "lifecycle", "run-context")
				if workErr := cw.RunContext(wctx); workErr != nil && !errors.Is(workErr, context.Canceled) {
					failComponent("worker "+w.Name(), workErr)
				}
				return
			}
			logger.Info("worker starting", "worker", w.Name())
			if startErr := w.Start(wctx); startErr != nil && !errors.Is(startErr, context.Canceled) {
				failComponent("worker "+w.Name(), startErr)
			}
		}(w)
	}

	// Start controller manager for operators (if any).
	//
	// Operator gating: the caller already decided WHICH operators to
	// populate into srv.Operators (the relocated name filter), so
	// serverkit only honours the process-wide RUN_OPERATORS opt-out.
	// Setting RUN_OPERATORS=false lets a catch-all API-server process run
	// no controller manager (and need no operator RBAC) while a separate
	// process runs the manager. With no operators populated, the manager
	// never starts — avoiding the misleading "not running in-cluster"
	// warning on a host process.
	if len(srv.Operators) > 0 && shouldRunOperators() {
		go func() {
			if opErr := srv.RunOperators(runCtx, logger, cfg.OperatorHealthProbeAddr); opErr != nil && !errors.Is(opErr, context.Canceled) {
				failComponent("controller manager", opErr)
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
	//   3. Begin httpSrv.Shutdown, bounded by shutdown_timeout, so in-flight
	//      requests drain without accepting new ones.
	// Without step 1 + 2, httpSrv.Shutdown stops accepting new conns
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
	for _, w := range srv.Workers {
		if stopErr := w.Stop(shutdownCtx); stopErr != nil {
			logger.Error("worker stop error", "worker", w.Name(), "error", stopErr)
		}
	}

	// OnShutdown is the caller-composed teardown (the old
	// Application.Shutdown). Runs after workers stop, before the OTel flush
	// and the http.Server shutdown.
	if srv.OnShutdown != nil {
		if shutErr := srv.OnShutdown(shutdownCtx); shutErr != nil {
			logger.Error("shutdown error", "error", shutErr)
		}
	}

	// OTel flush — serverkit owns it now (folded out of the cmd shim).
	if otelShutdown != nil {
		if shutErr := otelShutdown(shutdownCtx); shutErr != nil {
			logger.Error("otel shutdown error", "error", shutErr)
		}
	}
	if shutErr := httpSrv.Shutdown(shutdownCtx); shutErr != nil {
		logger.Error("server shutdown error", "error", shutErr)
	}
	if pprofSrv != nil {
		if shutErr := pprofSrv.Shutdown(shutdownCtx); shutErr != nil {
			logger.Error("pprof shutdown error", "error", shutErr)
		}
	}

	// A component failure (worker/operator under FailProcess) initiated
	// this shutdown: surface it as Run's return so the process exits
	// non-zero and the platform supervisor restarts it loudly.
	componentMu.Lock()
	if runErr == nil && componentFailure != nil {
		runErr = componentFailure
	}
	componentMu.Unlock()
	return runErr
}

// NewLogger builds the slog.Logger serverkit would use from Config:
// LOG_FORMAT picks between structured JSON (for log aggregators) and
// human-friendly text (for local dev tails); anything other than "text"
// emits JSON. Exported so the cmd layer — which now composes the server
// and bootstraps BEFORE calling Run — can build the SAME logger and pass
// it as Server.Logger, keeping mount-time and run-time logs consistent.
func NewLogger(cfg Config) *slog.Logger { return newLogger(cfg) }

// newLogger builds the slog.Logger Run dispatches on.
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
// start. Service SELECTION moved above serverkit — the caller only
// populates Server.Operators for processes that should run the manager —
// so serverkit's only remaining gate is the process-wide RUN_OPERATORS
// opt-out.
//
// Setting RUN_OPERATORS=false lets a catch-all API-server Deployment run
// no manager (and need no operator RBAC) even if its build happens to
// link operators, while a separate operator process runs the manager.
// Default behaviour is unchanged when the var is unset/empty, so
// existing projects are unaffected.
func shouldRunOperators() bool {
	return runOperatorsEnvDefault()
}

// runOperatorsEnvDefault reports whether operators are permitted to run
// at all, honouring the RUN_OPERATORS opt-out. Only an explicit false
// value ("false"/"0"/"no"/"off", case-insensitive) disables operators;
// unset/empty/any other value preserves the default-on behaviour so
// existing deployments are unaffected.
func runOperatorsEnvDefault() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RUN_OPERATORS"))) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}
