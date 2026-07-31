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
	cfg.Normalize()

	// Handler is required, but the Mux ergonomics let a composition root
	// leave it nil and rely on Server.Mux as the handler (the common case:
	// no REST swap, no outer wrapper). Fall back to Mux before failing.
	if srv.Handler == nil {
		if srv.Mux != nil {
			srv.Handler = srv.Mux
		} else {
			return fmt.Errorf("serverkit.Run: Server.Handler is required (or set Server.Mux)")
		}
	}
	if cfg.Addr == "" {
		return fmt.Errorf("serverkit.Run: Config.Addr is required")
	}
	if len(srv.Operators) > 0 && srv.RunOperators == nil {
		return fmt.Errorf("serverkit.Run: Server.Operators is non-empty but Server.RunOperators is nil")
	}

	// Fail closed on configured-but-unwired edge layers. CORS and security
	// headers are SECURITY controls: a composition root that configures
	// them (origins set, SecurityHeaders=true) but forgets to supply the
	// factory must NOT silently serve with the layer absent. Reject
	// loudly instead.
	if cfg.CORSEnabled() && srv.CORSMiddleware == nil {
		return fmt.Errorf("serverkit.Run: CORS is enabled (origins set, or Environment=%s) but Server.CORSMiddleware is nil — refusing to serve with CORS unenforced", EnvDevelopment)
	}
	if cfg.SecurityHeaders && srv.SecurityHeadersMiddleware == nil {
		return fmt.Errorf("serverkit.Run: Config.SecurityHeaders is enabled but Server.SecurityHeadersMiddleware is nil — refusing to serve without security headers")
	}

	// Logger: prefer the caller's (so mount-time and run-time logs share
	// attrs); build one from Config only when the caller passed nil.
	logger := srv.Logger
	if logger == nil {
		logger = newLogger(cfg)
	}
	slog.SetDefault(logger)

	// Warn loudly when running in development mode. Development mode enables
	// permissive defaults (e.g. origin-reflecting CORS, security headers off)
	// for local ergonomics — this must never be used in production. The
	// permissive behavior is selected by the caller (at mount time) based on
	// cfg.Environment.
	if cfg.IsDevelopment() {
		logger.Warn("running in development mode — permissive edge defaults are enabled (CORS reflects any origin, HSTS off). NEVER set ENVIRONMENT=development in production.")
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

	// Readiness gate: flipped true AFTER the listener binds (see ln/Serve
	// below) and false again at the top of shutdown. It is only the FIRST
	// half of /readyz's answer — the dependency checks in srv.ReadyChecks
	// are the other half, and they run per request.
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
	// logic. serverkit only owns the gating (cfg.CORSEnabled: origins
	// named, OR development — see the method's doc for why dev needs no
	// origins). A configured-but-unwired factory was already rejected
	// above (fail closed), so a non-nil CORSMiddleware here is guaranteed.
	if cfg.CORSEnabled() {
		handler = srv.CORSMiddleware(cfg.CORSOrigins, cfg.CORSAllowCredentials)(handler)
	}

	// Security headers — OWASP defaults via project middleware. HSTS is
	// only emitted in production (Environment != "development"). As with
	// CORS, a configured-but-unwired factory was rejected above.
	if cfg.SecurityHeaders {
		handler = srv.SecurityHeadersMiddleware(!cfg.IsDevelopment())(handler)
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
	// /readyz is the ROUTABILITY answer: listener bound, not draining, and
	// every registered dependency reachable. It is what a rolling deploy
	// believes, so it runs the checks live on each request rather than
	// reporting a value latched at boot. See readiness.go.
	top.HandleFunc("/readyz", readyzHandler(&ready, srv.ReadyChecks, cfg.ReadinessTimeout, logger))
	// /flow-health: the APP-FLOW assertion endpoint. Mounted only when the
	// caller registered FlowChecks. Like /healthz + /readyz it sits IN FRONT
	// of the edge (no CORS/auth) and is STATUS-ONLY — 200 when every check
	// passes, 503 when any fails, plus a terse non-sensitive aggregate. This
	// is what `forge env smoke` curls to catch green-while-broken app flows.
	if len(srv.FlowChecks) > 0 {
		top.HandleFunc("/flow-health", flowHealthHandler(srv.FlowChecks))
	}
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
			// The main httpSrv goroutine is ALREADY serving by now, so a
			// bare return here would leak the main listener/accept-loop and
			// skip the OTel flush. Route the bind failure through the normal
			// shutdown path instead: record it as a component failure and
			// cancel runCtx so the select below drops straight into graceful
			// shutdown and Run returns this error.
			pprofSrv = nil // nothing to Shutdown — Serve never ran
			failComponent("pprof listener", fmt.Errorf("listen pprof %s: %w", cfg.PprofAddr, lnErr))
		} else {
			go func() {
				logger.Info("pprof server starting", "addr", cfg.PprofAddr)
				if serveErr := pprofSrv.Serve(pprofLn); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
					logger.Error("pprof server error", "error", serveErr)
				}
			}()
		}
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
	// Operator gating is the caller's, entirely: whether this process runs
	// a controller manager is decided by whether it populated
	// srv.Operators, in the composition root, in code. serverkit adds no
	// gate of its own — a deployment that must not run a manager runs the
	// command that populates no operators, so the same binary never
	// behaves differently for reasons its own source does not show.
	//
	// The operator goroutine is supervised on operatorWg — the SAME
	// WaitGroup pattern as workers — so Run WAITS for RunOperators to drain
	// before returning. Without this wait, runCtx cancellation on shutdown
	// would let Run return mid-reconcile (the process could exit before
	// controller-runtime finishes its own ctx-cancel drain) AND a
	// failComponent write that lands after the final componentFailure read
	// would be silently lost.
	var operatorWg sync.WaitGroup
	operatorsSupervised := len(srv.Operators) > 0
	if operatorsSupervised {
		operatorWg.Add(1)
		go func() {
			defer operatorWg.Done()
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

	// TEARDOWN ORDER, innermost dependency last. Every step below is
	// something a step ABOVE it may still be using:
	//
	//  1. Drain in-flight HTTP. Those requests are still reading the DB and
	//     handing work to workers, so nothing they depend on may be torn
	//     down first.
	//  2. Stop workers, then operators.
	//  3. OnShutdown — the caller's teardown, which is where the serving
	//     *sql.DB gets closed. Closing it before (1) would fail the very
	//     requests the drain exists to complete.
	//  4. Flush telemetry LAST, so the drain window's own spans, metrics
	//     and error records are included. Flushing before the drain exported
	//     everything up to that instant and then discarded whatever the
	//     shutdown itself produced — precisely the window an operator reads
	//     when a deploy goes wrong.
	if shutErr := httpSrv.Shutdown(shutdownCtx); shutErr != nil {
		logger.Error("server shutdown error", "error", shutErr)
	}

	workerCancel()
	if len(srv.Workers) > 0 && !waitWithin(shutdownCtx, &workerWg) {
		logger.Error("workers did not drain within the shutdown budget",
			"shutdown_timeout", cfg.ShutdownTimeout)
	}
	for _, w := range srv.Workers {
		if stopErr := w.Stop(shutdownCtx); stopErr != nil {
			logger.Error("worker stop error", "worker", w.Name(), "error", stopErr)
		}
	}

	// Wait for the operator/controller-manager goroutine to drain. runCtx
	// is already cancelled (it's what dropped us out of the serve select),
	// so RunOperators is unwinding controller-runtime's own ctx-cancel
	// drain; this Wait ensures Run does not return before that finishes and
	// that any failComponent write from RunOperators lands before the final
	// componentFailure read below. Bounded by shutdownCtx: an operator that
	// never returns must not hold the process past ShutdownTimeout, because
	// the platform's own grace period is about to SIGKILL it anyway and a
	// clean-ish exit beats being shot mid-flush.
	if operatorsSupervised && !waitWithin(shutdownCtx, &operatorWg) {
		logger.Error("controller manager did not drain within the shutdown budget",
			"shutdown_timeout", cfg.ShutdownTimeout)
	}

	// OnShutdown is the caller-composed teardown (closing the serving DB
	// pool, flushing app-owned buffers). Runs after HTTP has drained and
	// workers have stopped, so nothing it closes is still in use.
	if srv.OnShutdown != nil {
		if shutErr := srv.OnShutdown(shutdownCtx); shutErr != nil {
			logger.Error("shutdown error", "error", shutErr)
		}
	}

	if pprofSrv != nil {
		if shutErr := pprofSrv.Shutdown(shutdownCtx); shutErr != nil {
			logger.Error("pprof shutdown error", "error", shutErr)
		}
	}

	// OTel flush — serverkit owns it now (folded out of the cmd shim). LAST,
	// so it exports everything the steps above produced.
	if otelShutdown != nil {
		if shutErr := otelShutdown(shutdownCtx); shutErr != nil {
			logger.Error("otel shutdown error", "error", shutErr)
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

// waitWithin blocks until wg drains or ctx expires, reporting whether the
// drain finished. sync.WaitGroup.Wait takes no context, so a bare Wait sits
// OUTSIDE ShutdownTimeout entirely: one worker that ignores its cancelled
// context pins the process until the platform SIGKILLs it, skipping every
// remaining teardown step — including the telemetry flush that would have
// explained what happened.
func waitWithin(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
	}
	// The deadline can expire in the same instant the group drains, and
	// select picks a ready case at random. Re-check before reporting a
	// timeout so a clean shutdown is never logged as a stuck one.
	select {
	case <-done:
		return true
	default:
		return false
	}
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
