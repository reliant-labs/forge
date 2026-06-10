package serverkit

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
)

// Application is the runtime shape serverkit needs from the project's
// generated *app.App. The codegen pkg/app/app_gen.go implements this
// interface; serverkit never imports pkg/app directly.
//
// A nil RESTHandler() return disables the REST transcoder swap — the
// raw mux is served instead. HasOperators() lets serverkit skip the
// controller-manager goroutine entirely on projects that declared no
// operator services, avoiding a misleading "not running in-cluster"
// warning on every startup.
type Application interface {
	// WorkerList returns every Worker the project declared. serverkit
	// fans them out in goroutines and supervises their Start/Stop.
	WorkerList() []Worker

	// OperatorList returns every Operator the project declared.
	// serverkit consults it for gating (see shouldRunOperators) but
	// hands off the actual manager wiring to RunOperators.
	OperatorList() []Operator

	// HasOperators is true when OperatorList is non-empty. Exposed as
	// a method (rather than computed from len(OperatorList())) so the
	// codegen App can answer it without allocating the list.
	HasOperators() bool

	// RunOperators starts the controller-runtime manager and blocks
	// until ctx is done. The healthProbeAddr argument carries the
	// Config.OperatorHealthProbeAddr value through — projects that
	// don't bind a probe listener can ignore it.
	RunOperators(ctx context.Context, logger *slog.Logger, healthProbeAddr string) error

	// RESTHandler returns the vanguard REST transcoder wrapped around
	// the Connect mux when api.rest is enabled in forge.yaml, or nil
	// when REST is disabled. serverkit substitutes the return value
	// for the raw mux in the handler chain so REST/gRPC/Connect share
	// the same CORS, security-headers, and request-id middleware.
	RESTHandler() http.Handler

	// Shutdown is invoked during the graceful-shutdown sequence after
	// workers have stopped and before the HTTP server's own Shutdown.
	// Projects that hold external resources (db pools, queue conns)
	// close them here.
	Shutdown(ctx context.Context) error
}

// Worker is the runtime contract for a long-running background task.
// The generated WorkerInstance in pkg/app satisfies it directly.
type Worker interface {
	// Name is the worker's stable identifier — used in log lines, the
	// `server [names...]` filter, and the WorkerList iteration order.
	Name() string

	// Start runs the worker's main loop. It must return when ctx is
	// done. serverkit calls Start once per worker in its own goroutine
	// and never restarts.
	Start(ctx context.Context) error

	// Stop performs graceful shutdown bounded by the supplied ctx
	// (typically Config.ShutdownTimeout). serverkit calls Stop after
	// the worker's Start ctx has been cancelled and the supervisor
	// WaitGroup has drained.
	Stop(ctx context.Context) error
}

// ContextWorker is an OPTIONAL extension of Worker for context-aware
// run loops. When a worker returned by Application.WorkerList also
// implements ContextWorker, the supervisor calls RunContext instead of
// Start, passing a per-worker context derived from the run lifecycle.
// Workers that don't implement it keep the legacy Start path unchanged
// — the supervisor type-asserts at fan-out time, so adopting the
// interface is purely additive.
//
// The shutdown contract:
//
//   - ctx is cancelled the moment graceful shutdown begins (SIGINT/
//     SIGTERM received, or a fatal serve error). RunContext must observe
//     ctx.Done() inside its cycle loop — and thread ctx into per-tick
//     work (DB queries, HTTP calls, adapters) — so an in-flight cycle is
//     interrupted rather than running to completion.
//   - RunContext must return promptly after cancellation. The supervisor
//     waits for every worker goroutine to exit before continuing the
//     shutdown sequence, so a worker that ignores ctx stalls shutdown.
//   - Returning nil or ctx.Err() after cancellation is a clean exit —
//     the supervisor does not log context.Canceled as a worker error.
//     Any other error is logged (never returned from Run, never
//     restarted), matching the legacy Start error handling.
//   - Stop is still called afterwards (every WorkerList entry is a
//     Worker), bounded by Config.ShutdownTimeout. For ContextWorker
//     implementations Stop is typically a final-drain no-op since
//     cancellation already unwound the run loop.
//
// Cron-scheduled and continuous workers both fit: a continuous worker
// selects on ctx.Done() in its cycle loop; a cron worker derives each
// tick's context from ctx so scheduled jobs observe shutdown mid-run.
type ContextWorker interface {
	Worker

	// RunContext runs the worker's main loop and returns when ctx is
	// done. See the interface doc for the full shutdown contract.
	RunContext(ctx context.Context) error
}

// Operator is the minimal contract serverkit needs to gate the
// controller-manager goroutine. The actual controller wiring happens
// inside Application.RunOperators — this interface only carries Name
// so the `server [names...]` filter can match.
type Operator interface {
	Name() string
}

// DBPoolTuning groups the four sql.DB pool knobs serverkit applies to
// the AutoMigrate connection. Values come from the project config so
// operators can tune them per environment without recompiling. A
// zero/empty field leaves the corresponding setting at Go's default.
type DBPoolTuning struct {
	// MaxOpenConns caps total open connections. 0 = unlimited.
	MaxOpenConns int

	// MaxIdleConns caps idle connections. 0 = default (2).
	MaxIdleConns int

	// ConnMaxIdleTime is how long an idle connection survives before
	// being closed. Empty string disables the limit.
	ConnMaxIdleTime time.Duration

	// ConnMaxLifetime is the absolute lifetime of any connection.
	// Empty string disables the limit.
	ConnMaxLifetime time.Duration
}

// Config bundles every knob Run reads from the project's *config.Config.
// The generated cmd/server.go shim projects its typed config onto this
// struct so serverkit has no compile-time dependency on the project's
// config package.
//
// Fields with zero values fall back to sensible defaults documented per
// field; Run never returns "missing required field" for any of these.
type Config struct {
	// Addr is the public listener address (e.g. ":8080"). Required.
	Addr string

	// PprofAddr is the side-listener for net/http/pprof. Empty
	// disables the pprof server entirely. Never mount pprof on the
	// public Addr — its endpoints can leak memory and stall the
	// process.
	PprofAddr string

	// TLSCertPath and TLSKeyPath enable TLS when both are non-empty.
	// Validation of "both or neither" must happen in the project's
	// config layer before Run is called.
	TLSCertPath string
	TLSKeyPath  string

	// LogLevel is the slog level applied to the root logger. Zero
	// value is slog.LevelInfo.
	LogLevel slog.Level

	// LogFormat selects the slog handler. "text" emits text;
	// anything else (including "" and "json") emits JSON.
	LogFormat string

	// Environment is the deployment environment string. When equal to
	// "development" Run emits a loud warning about permissive defaults.
	Environment string

	// AutoMigrate triggers the Hooks.AutoMigrate callback when true.
	// The project supplies the actual migration body.
	AutoMigrate bool

	// DatabaseURL is the DSN passed to sql.Open for the AutoMigrate
	// connection. Required when AutoMigrate is true.
	DatabaseURL string

	// DBDriver is the sql.Open driver name (e.g. "pgx"). Empty
	// defaults to "pgx".
	DBDriver string

	// DBPoolTuning is applied to the AutoMigrate connection before the
	// migration runs.
	DBPoolTuning DBPoolTuning

	// CORSOrigins is the allow-list applied to inbound requests when
	// non-empty. Each entry must be a full origin (scheme + host +
	// optional port).
	CORSOrigins []string

	// CORSAllowCredentials toggles Access-Control-Allow-Credentials.
	// The wildcard-origin + credentials combination must be rejected
	// by the project's config validation before Run is called.
	CORSAllowCredentials bool

	// SecurityHeaders enables the OWASP security-header middleware.
	// HSTS is only emitted when Environment != "development".
	SecurityHeaders bool

	// PreStopDelay is the readiness-flip drain pause. Zero falls back
	// to 5s. Operators tune this to match their load-balancer probe
	// interval — too short causes brief 502s on rollout.
	PreStopDelay time.Duration

	// ShutdownTimeout bounds the post-readiness-flip shutdown window
	// (worker Stop, Application.Shutdown, http.Server.Shutdown all
	// share this budget). Zero falls back to 30s.
	ShutdownTimeout time.Duration

	// ReadMaxBytes caps the size of a single Connect request payload.
	// Zero falls back to 4 MiB.
	ReadMaxBytes int

	// SendMaxBytes caps the size of a single Connect response payload.
	// Zero falls back to 4 MiB.
	SendMaxBytes int

	// OperatorHealthProbeAddr is forwarded to Application.RunOperators
	// for projects (like cp-forge) that bind a /healthz + /readyz
	// listener inside the controller manager. Empty string leaves the
	// project's RunOperators to fall back to its own default.
	OperatorHealthProbeAddr string
}

// Hooks are the project-typed callbacks Run dispatches to. Everything
// else in the lifecycle is uniform and owned by serverkit.
//
// Bootstrap is required; all other fields are optional. A nil hook is
// treated as a no-op (the corresponding lifecycle step is skipped).
type Hooks struct {
	// Bootstrap constructs the project's Application and mounts its
	// Connect handlers on mux. When names is non-empty, only services
	// in that set should be registered — the generated shim typically
	// dispatches to app.BootstrapOnly vs app.Bootstrap based on the
	// length. Bootstrap must return a non-nil Application on success.
	Bootstrap func(
		ctx context.Context,
		mux *http.ServeMux,
		logger *slog.Logger,
		names []string,
		opts ...connect.HandlerOption,
	) (Application, error)

	// PostBootstrap is the single forge-blessed chokepoint for wiring
	// that depends on a constructed component (e.g. assigning a worker
	// collaborator after both have been built). Runs after Bootstrap,
	// before the listener binds. An error here aborts startup. The
	// generated shim typically delegates to app.PostBootstrap with a
	// type assertion back to *app.App.
	PostBootstrap func(app Application) error

	// AutoMigrate is invoked when Config.AutoMigrate is true. serverkit
	// dials the database, applies Config.DBPoolTuning, calls the hook,
	// and closes the connection. The hook receives the open *sql.DB so
	// it can run whatever migration tool the project chose.
	AutoMigrate func(ctx context.Context, db *sql.DB, logger *slog.Logger) error

	// SetupOTel initializes the project's OpenTelemetry pipeline and
	// returns a shutdown function plus the /metrics handler. A nil
	// hook leaves OTel disabled and serves no /metrics endpoint. A
	// non-nil error is logged but does not abort startup — projects
	// that depend on OTel for production are expected to fail Validate
	// on missing config before Run is called.
	SetupOTel func(ctx context.Context) (shutdown func(context.Context) error, metricsHandler http.Handler, err error)

	// ProjectInterceptors returns the project-specific Connect
	// interceptors appended to observe.DefaultMiddlewares. The
	// canonical chain (recovery → request-id → logging → tracing →
	// metrics) runs first; the returned slice runs in supplied order
	// AFTER it, in front of the handler. Use this for auth, audit,
	// rate-limit, otelconnect, and similar project-owned layers.
	ProjectInterceptors func(logger *slog.Logger) []connect.Interceptor

	// HTTPMiddleware wraps the final http.Handler after serverkit has
	// applied its own stack (RESTHandler swap, CORS, security headers,
	// request-id, h2c). Use this for project-specific outer wrappers
	// the canonical chain doesn't know about. A nil hook leaves the
	// handler unwrapped.
	HTTPMiddleware func(http.Handler) http.Handler

	// CORSMiddleware is OPTIONAL. When non-nil and Config.CORSOrigins
	// is non-empty, serverkit calls it with the origins + creds flag to
	// build the CORS wrapper. nil leaves the handler unchanged — CORS
	// is project-owned because the existing middleware lives in the
	// generated pkg/middleware tree.
	CORSMiddleware func(origins []string, allowCredentials bool) func(http.Handler) http.Handler

	// SecurityHeadersMiddleware is OPTIONAL. When non-nil and
	// Config.SecurityHeaders is true, serverkit calls it with the
	// "production" flag (computed from Config.Environment) to build
	// the security-headers wrapper. nil leaves the handler unchanged.
	SecurityHeadersMiddleware func(production bool) func(http.Handler) http.Handler

	// RequestIDMiddleware is OPTIONAL. When non-nil, serverkit wraps
	// the handler with it just inside the h2c layer so every inner
	// middleware sees the correlation header. nil falls back to a
	// passthrough — most projects supply the scaffolded
	// middleware.RequestIDMiddleware here.
	RequestIDMiddleware func() func(http.Handler) http.Handler
}

// defaults projects unset Config fields onto their fallback values.
// Run calls this once at entry so the rest of the lifecycle reads a
// fully-populated Config.
func (c *Config) defaults() {
	if c.PreStopDelay == 0 {
		c.PreStopDelay = 5 * time.Second
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 30 * time.Second
	}
	if c.ReadMaxBytes == 0 {
		c.ReadMaxBytes = 4 << 20
	}
	if c.SendMaxBytes == 0 {
		c.SendMaxBytes = 4 << 20
	}
	if c.DBDriver == "" {
		c.DBDriver = "pgx"
	}
}
