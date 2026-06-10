package appkit

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"connectrpc.com/vanguard"

	"github.com/reliant-labs/forge/pkg/diagnostics"
)

// WorkerInstance wraps a worker with its lifecycle methods. The
// generated pkg/app re-exports it (`type WorkerInstance =
// appkit.WorkerInstance`) and builds the WorkerList table with
// [NewWorkerInstance]; cmd/server.go drives Start/Stop.
type WorkerInstance struct {
	name  string
	start func(ctx context.Context) error
	stop  func(ctx context.Context) error
}

// NewWorkerInstance builds a WorkerInstance row from a worker's name
// and Start/Stop methods.
func NewWorkerInstance(name string, start, stop func(ctx context.Context) error) *WorkerInstance {
	return &WorkerInstance{name: name, start: start, stop: stop}
}

// Name returns the worker's identifier.
func (w *WorkerInstance) Name() string { return w.name }

// Start blocks until ctx is cancelled.
func (w *WorkerInstance) Start(ctx context.Context) error { return w.start(ctx) }

// Stop is called during graceful shutdown.
func (w *WorkerInstance) Stop(ctx context.Context) error { return w.stop(ctx) }

// DiagnosticsMode selects how unwired-scaffold diagnostics recorded by
// the codegen pipeline (pkg/app/diagnostics_gen.go init()) are emitted
// after Setup runs. The generated table sets this from forge.yaml's
// `features.diagnostics` / `features.strict_wiring` toggles.
type DiagnosticsMode int

const (
	// DiagnosticsOff skips the diagnostics boot entirely (default —
	// existing projects don't suddenly start logging warns on regen).
	DiagnosticsOff DiagnosticsMode = iota
	// DiagnosticsLog emits one structured warn log line per registered
	// diagnostic, then a summary roll-up.
	DiagnosticsLog
	// DiagnosticsStrict wraps the log emitter in a StrictEmitter, which
	// os.Exit(1)s after the summary when any diagnostic is registered
	// (production-grade enforcement).
	DiagnosticsStrict
)

// Mounter registers a constructed service's handlers on the mux. The
// generated [ServiceDef].Construct closures return one so construction
// and mounting stay separable: BootstrapOnly's name filter applies to
// mounting only.
type Mounter func(mux *http.ServeMux)

// ServiceDef is one generated service row.
type ServiceDef struct {
	// Name is the runtime (kebab-case) service name — the filter key
	// matched against [Options].Only and the value cobra subcommands
	// pass through cmd/server.go.
	Name string
	// ConnectName is the Connect service path constant from the
	// connect-generated package (e.g. apiv1connect.APIServiceName).
	// Only populated when the project enables `api.rest: true`; it
	// feeds vanguard.NewService when [Def].REST is set. Empty
	// otherwise.
	ConnectName string
	// Construct wires the service's Deps (via the codegen'd
	// wireXxxDeps), calls New, assigns the instance onto the project's
	// App, and returns the Mounter that registers the service's
	// Connect + HTTP routes. Construction errors should be returned
	// pre-wrapped ("initializing <pkg> service: %w") so the historical
	// error strings survive the table migration.
	Construct func() (Mounter, error)
}

// WorkerDef is one generated worker row. Construct wires deps,
// constructs the worker, and assigns it onto app.Workers.<X>.
// Construction passes through [Hooks].ConstructWorker when set.
type WorkerDef struct {
	Name      string
	Construct func() error
}

// OperatorDef is one generated operator-controller row. Construct
// wires deps, constructs the controller, and assigns it onto
// app.Operators.<X>. Registration with the controller manager happens
// later via the generated App.RunOperators (see appkit/operatorkit).
type OperatorDef struct {
	Name      string
	Construct func() error
}

// PackageDef is one generated internal-package row. Packages are
// constructed before services, in table order, because services may
// depend on them.
type PackageDef struct {
	Name      string
	Construct func() error
}

// MountDef is one extra HTTP route to register on the mux after the
// generated service mounts. The escape hatch for hand-rolled endpoints
// that previously required forking bootstrap.go.
type MountDef struct {
	Pattern string
	Handler http.Handler
}

// Hooks customizes [Run]'s orchestration without touching the
// generated table. The generated App struct carries a value of this
// type; populate it in the user-owned pkg/app/setup.go (hooks are read
// after Setup returns). All fields are optional.
type Hooks struct {
	// BeforeMount runs after every service is constructed and before
	// the first generated service mount. A returned error aborts
	// bootstrap.
	BeforeMount func(mux *http.ServeMux) error
	// AfterMount runs after the generated service mounts and
	// ExtraMounts, before workers are constructed and before the REST
	// transcoder wraps the mux. A returned error aborts bootstrap.
	AfterMount func(mux *http.ServeMux) error
	// ExtraMounts are registered on the mux (in slice order) right
	// after the generated service mounts.
	ExtraMounts []MountDef
	// ConstructWorker intercepts each worker row's construction. It
	// receives the worker's table name and the row's default
	// constructor; call construct() to keep the generated behavior, or
	// skip it and assign your own instance onto app.Workers.<X>.
	ConstructWorker func(name string, construct func() error) error
}

// RESTDef enables REST transcoding (forge.yaml `api.rest: true`). Run
// builds a vanguard transcoder over the mux from every service row's
// ConnectName and hands the wrapped handler to Assign — the generated
// table points Assign at app.RESTHandler, which cmd/server.go serves in
// place of the bare mux when non-nil.
type RESTDef struct {
	Assign func(http.Handler)
}

// Def is the project's component table, generated into
// pkg/app/bootstrap.go by `forge generate`. Rows are dumb: names,
// constants, and type-capturing closures. All behavior lives in [Run].
type Def struct {
	// Setup is the user-owned Setup(app, cfg) hook — builds
	// infrastructure and assigns it onto *App fields before any
	// component is constructed.
	Setup func() error
	// Hooks returns the project's [Hooks]. It is invoked AFTER Setup so
	// hook assignments made in setup.go are observed. Nil (or a nil
	// return) means no hooks.
	Hooks func() *Hooks
	// Diagnostics selects the post-Setup diagnostics boot mode.
	Diagnostics DiagnosticsMode

	Packages  []PackageDef
	Services  []ServiceDef
	Workers   []WorkerDef
	Operators []OperatorDef

	// REST is non-nil when the project enables `api.rest: true`.
	REST *RESTDef
}

// Options carries per-invocation knobs (the generated Bootstrap /
// BootstrapOnly pass these through).
type Options struct {
	// Only filters which services get MOUNTED on the mux. Empty mounts
	// everything (the generated Bootstrap delegates with nil). Unknown
	// names are warned about and ignored. Workers and operators are
	// always constructed; the caller gates their start-up separately.
	Only []string
	// LazyConstruct additionally skips CONSTRUCTION of services
	// filtered out by Only (`binary: shared` semantics). Default false:
	// construct everything, filter at mount time.
	LazyConstruct bool
}

// Run executes the def table against the mux. See the package
// documentation for the exact orchestration order. The first error
// aborts and is returned as-is (row closures pre-wrap their errors).
func Run(def Def, mux *http.ServeMux, logger *slog.Logger, opts Options) error {
	if logger == nil {
		logger = slog.Default()
	}

	// Filter bookkeeping + unknown-name warning. runAll preserves the
	// "Bootstrap == BootstrapOnly with empty filter" identity.
	runAll := len(opts.Only) == 0
	selected := make(map[string]bool, len(opts.Only))
	for _, n := range opts.Only {
		selected[n] = true
	}
	if !runAll {
		known := make(map[string]bool)
		for _, s := range def.Services {
			known[s.Name] = true
		}
		for _, w := range def.Workers {
			known[w.Name] = true
		}
		for _, o := range def.Operators {
			known[o.Name] = true
		}
		var knownNames []string
		for n := range known {
			knownNames = append(knownNames, n)
		}
		sort.Strings(knownNames)
		for _, n := range opts.Only {
			if !known[n] {
				logger.Warn("unknown service/worker/operator name, ignoring", "name", n, "known", knownNames)
			}
		}
	}

	// 1. User-owned Setup — infrastructure construction + App field
	// assignment (including hook population).
	if def.Setup != nil {
		if err := def.Setup(); err != nil {
			return fmt.Errorf("setup: %w", err)
		}
	}

	// 2. Diagnostics boot — after Setup so the operator reads the
	// warnings before any service/worker starts.
	switch def.Diagnostics {
	case DiagnosticsLog:
		diagnostics.Default.Boot(diagnostics.NewLogEmitter(logger))
	case DiagnosticsStrict:
		diagnostics.Default.Boot(diagnostics.NewStrictEmitter(diagnostics.NewLogEmitter(logger)))
	}

	// Hooks are read after Setup so setup.go assignments are observed.
	var hooks Hooks
	if def.Hooks != nil {
		if h := def.Hooks(); h != nil {
			hooks = *h
		}
	}

	// 3. Internal packages (services may depend on them).
	for _, p := range def.Packages {
		if err := p.Construct(); err != nil {
			return err
		}
	}

	// 4. Construct services. Mounting is deferred so the Only filter
	// applies to registration, not construction (unless LazyConstruct).
	type mountRow struct {
		name  string
		mount Mounter
	}
	var mounts []mountRow
	for _, s := range def.Services {
		if opts.LazyConstruct && !runAll && !selected[s.Name] {
			continue
		}
		m, err := s.Construct()
		if err != nil {
			return err
		}
		mounts = append(mounts, mountRow{name: s.Name, mount: m})
	}

	// 5. Mount phase: BeforeMount -> generated mounts (filtered) ->
	// ExtraMounts -> AfterMount.
	if hooks.BeforeMount != nil {
		if err := hooks.BeforeMount(mux); err != nil {
			return err
		}
	}
	for _, m := range mounts {
		if m.mount == nil {
			continue
		}
		if runAll || selected[m.name] {
			m.mount(mux)
		}
	}
	for _, em := range hooks.ExtraMounts {
		mux.Handle(em.Pattern, em.Handler)
	}
	if hooks.AfterMount != nil {
		if err := hooks.AfterMount(mux); err != nil {
			return err
		}
	}

	// 6. Workers — always constructed (they're cheap; the caller gates
	// which ones START). ConstructWorker interception is the documented
	// replacement for the hand-rolled constructWorkers forks.
	for _, w := range def.Workers {
		if hooks.ConstructWorker != nil {
			if err := hooks.ConstructWorker(w.Name, w.Construct); err != nil {
				return err
			}
			continue
		}
		if err := w.Construct(); err != nil {
			return err
		}
	}

	// 7. Operators — constructed here; registered with the controller
	// manager later via the generated App.RunOperators.
	for _, o := range def.Operators {
		if err := o.Construct(); err != nil {
			return err
		}
	}

	// 8. REST transcoding: vanguard wraps the Connect mux and
	// translates REST<->Connect at runtime based on google.api.http
	// annotations. Built over ALL service rows (not just mounted ones),
	// matching the historical generated behavior.
	if def.REST != nil && def.REST.Assign != nil {
		var vanguardSvcs []*vanguard.Service
		for _, s := range def.Services {
			if s.ConnectName == "" {
				continue
			}
			vanguardSvcs = append(vanguardSvcs, vanguard.NewService(s.ConnectName, mux))
		}
		if len(vanguardSvcs) > 0 {
			transcoder, terr := vanguard.NewTranscoder(vanguardSvcs)
			if terr != nil {
				return fmt.Errorf("init vanguard REST transcoder: %w", terr)
			}
			def.REST.Assign(transcoder)
		}
	}

	return nil
}
