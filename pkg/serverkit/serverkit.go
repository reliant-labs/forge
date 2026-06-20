package serverkit

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Server carries the already-composed inputs serverkit runs. Service
// SELECTION — which handlers/workers/operators this process serves — has
// happened ABOVE serverkit: the caller (the generated cmd-server shim)
// builds the mux, mounts the selected services on it, constructs the
// selected workers/operators, and hands the result here. serverkit owns
// only the uniform lifecycle (listener bind, the HTTP edge, health
// probes, worker supervision, operator gating, graceful shutdown) and
// knows nothing about names.
type Server struct {
	// Handler is the fully-composed HTTP handler: the mux with all
	// services already mounted (their Connect interceptors already
	// applied via connect.HandlerOption at mount time), plus any
	// REST-transcoder swap already resolved by the caller, plus any
	// /metrics handle the caller mounted. serverkit wraps it with its
	// OWN edge (CORS/security/request-id/h2c from Config + the factory
	// fields below) and routes /healthz + /readyz to its own probes
	// IN FRONT of that edge, but never re-mounts services. Required.
	Handler http.Handler

	// Logger is the root logger the caller already built (the same one
	// it passed into bootstrap so mount-time logs and run-time logs
	// agree). When nil, serverkit builds one from Config (newLogger) and
	// SetDefaults it. Optional.
	Logger *slog.Logger

	// Workers / Operators are the already-constructed, already-selected
	// supervised components. Selection (the old names filter) happened
	// ABOVE serverkit: the caller passes exactly the workers/operators
	// this process should run. serverkit no longer filters by name.
	Workers   []Worker
	Operators []Operator

	// OnShutdown runs during graceful shutdown after workers stop and
	// before the http.Server shuts down — the old Application.Shutdown
	// plus (folded in by the caller) the OTel flush. Optional; nil is a
	// no-op.
	OnShutdown func(context.Context) error

	// RunOperators starts the controller-runtime manager and blocks
	// until ctx is done. serverkit calls it in a goroutine when
	// len(Operators) > 0 AND the RUN_OPERATORS env gate allows. The old
	// per-name operator gating is gone: the caller already decided
	// whether to populate Operators, so serverkit only honours the
	// process-wide RUN_OPERATORS opt-out. Nil + non-empty Operators is a
	// config error. Optional when Operators is empty.
	RunOperators func(ctx context.Context, logger *slog.Logger, healthProbeAddr string) error

	// Edge factories: kept as fields (not pure Config) because the
	// concrete middleware still lives in the project's generated
	// pkg/middleware tree and serverkit must not import the project.
	// serverkit owns the GATING (driven by Config: CORSOrigins,
	// SecurityHeaders, Environment); these only supply the wrapper. All
	// optional — nil skips that edge layer.
	CORSMiddleware            func(origins []string, allowCredentials bool) func(http.Handler) http.Handler
	SecurityHeadersMiddleware func(production bool) func(http.Handler) http.Handler
	RequestIDMiddleware       func() func(http.Handler) http.Handler
	HTTPMiddleware            func(http.Handler) http.Handler
}

// Worker is the runtime contract for a long-running background task.
// The generated WorkerInstance in pkg/app satisfies it directly.
type Worker interface {
	// Name is the worker's stable identifier — used in log lines and
	// the Server.Workers iteration order.
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
// run loops. When a worker in Server.Workers also
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
//     Any other error is dispatched on Config.FailurePolicy: FailProcess
//     (the default) terminates the process with the worker's error;
//     Ignore logs and continues. Workers are never restarted in-process
//     — restart is the platform supervisor's job.
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

// FailurePolicy governs what Run does when a supervised background
// component fails: a worker's Start/RunContext returning a
// non-cancellation error, or Server.RunOperators returning an
// error. The zero value is [FailProcess] — fail loud. A pod that
// restarts with a clear error in its termination log is operable; a pod
// that keeps serving HTTP while its workers are silently dead is a
// data-loss incident with a delay timer.
type FailurePolicy int

const (
	// FailProcess (the zero value / default) cancels the run context on
	// the first component failure: graceful shutdown executes and Run
	// returns the component's error, so the supervisor (Kubernetes,
	// systemd, …) restarts the process loudly.
	FailProcess FailurePolicy = 0

	// Ignore logs the component error and keeps serving — the pre-2026
	// behaviour. Explicit opt-in for deployments where a worker is
	// genuinely best-effort and a dead one must not take down the API.
	Ignore FailurePolicy = 1
)

// Operator is the minimal contract serverkit needs to count the
// supervised operators. The actual controller wiring happens inside
// Server.RunOperators — this interface only carries Name for log lines.
type Operator interface {
	Name() string
}

// DBPoolTuning groups the four sql.DB pool knobs the caller applies to
// its migration connection via ApplyDBPoolTuning. Values come from the
// project config so operators can tune them per environment without
// recompiling. A zero/empty field leaves the corresponding setting at
// Go's default.
type DBPoolTuning struct {
	// MaxOpenConns caps total open connections. 0 = unlimited.
	MaxOpenConns int

	// MaxIdleConns caps idle connections. 0 = default (2).
	MaxIdleConns int

	// ConnMaxIdleTime is how long an idle connection survives before
	// being closed. Zero disables the limit.
	ConnMaxIdleTime time.Duration

	// ConnMaxLifetime is the absolute lifetime of any connection.
	// Zero disables the limit.
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

	// AutoMigrate signals the caller-owned migration step should run.
	// serverkit no longer runs migration itself — the cmd layer reads
	// this flag (plus DatabaseURL/DBDriver/DBPoolTuning below) and runs
	// the migration before calling Run. The fields remain on Config so
	// the projection from the project's typed config stays in one place.
	AutoMigrate bool

	// DatabaseURL is the DSN the caller passes to sql.Open for its
	// migration connection. Required (caller-side) when AutoMigrate is
	// true.
	DatabaseURL string

	// DBDriver is the sql.Open driver name (e.g. "pgx"). Empty
	// defaults to "pgx".
	DBDriver string

	// DBPoolTuning is applied (via ApplyDBPoolTuning) by the caller to
	// its migration connection before the migration runs.
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
	// (worker Stop, Server.OnShutdown, http.Server.Shutdown all
	// share this budget). Zero falls back to 30s.
	ShutdownTimeout time.Duration

	// ReadMaxBytes caps the size of a single Connect request payload.
	// Zero falls back to 4 MiB.
	ReadMaxBytes int

	// SendMaxBytes caps the size of a single Connect response payload.
	// Zero falls back to 4 MiB.
	SendMaxBytes int

	// OperatorHealthProbeAddr is forwarded to Server.RunOperators
	// for projects (like cp-forge) that bind a /healthz + /readyz
	// listener inside the controller manager. Empty string leaves the
	// project's RunOperators to fall back to its own default.
	OperatorHealthProbeAddr string

	// FailurePolicy governs worker Start/RunContext errors and
	// RunOperators errors. Zero value is FailProcess (a component
	// failure terminates the process loudly); set Ignore to restore
	// log-and-continue. See the FailurePolicy doc.
	FailurePolicy FailurePolicy
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
