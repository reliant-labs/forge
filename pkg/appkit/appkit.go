package appkit

import (
	"context"

	"github.com/reliant-labs/forge/pkg/serverkit"
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

// ContextWorkerInstance is the ctx-aware sibling of [WorkerInstance]:
// it additionally exposes RunContext, so a value of this type satisfies
// serverkit.ContextWorker and the serverkit supervisor prefers the
// ctx-aware lifecycle (per-worker cancel-on-shutdown) over legacy
// Start. It is a SEPARATE type — not an always-present RunContext on
// WorkerInstance that sometimes delegates to Start — because the
// supervisor's preference is a type assertion: a universally-present
// RunContext would make every worker look ctx-aware and silently change
// legacy workers' lifecycle.
type ContextWorkerInstance struct {
	WorkerInstance
	runContext func(ctx context.Context) error
}

// NewContextWorkerInstance builds a ContextWorkerInstance from a
// worker's name and its RunContext/Stop methods. The embedded
// WorkerInstance's Start delegates to runContext — semantically
// identical for a ctx-aware worker (both receive the supervisor's
// per-worker ctx), and the supervisor never takes the Start path when
// RunContext is present anyway; Start exists only so the wrapper still
// satisfies the base serverkit.Worker interface.
func NewContextWorkerInstance(name string, runContext, stop func(ctx context.Context) error) *ContextWorkerInstance {
	return &ContextWorkerInstance{
		WorkerInstance: WorkerInstance{name: name, start: runContext, stop: stop},
		runContext:     runContext,
	}
}

// RunContext runs the worker's ctx-aware main loop. See
// serverkit.ContextWorker for the full shutdown contract.
func (w *ContextWorkerInstance) RunContext(ctx context.Context) error { return w.runContext(ctx) }

// WorkerLifecycle is the Start/Stop surface every generated worker type
// exposes — the serverkit.Worker shape minus Name, which the generated
// table supplies from forge.yaml rather than from the worker itself.
type WorkerLifecycle interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// contextRunner mirrors the RunContext half of serverkit.ContextWorker
// for the WrapWorker type-switch.
type contextRunner interface {
	RunContext(ctx context.Context) error
}

// WrapWorker wraps a constructed worker for the serverkit supervisor.
// It is the single helper the generated WorkerList rows call — a
// runtime type-switch (chosen over codegen-time AST detection so the
// generated table stays one dumb call per row and adding RunContext to
// a worker needs no `forge generate`):
//
//   - the worker implements RunContext(ctx) → *ContextWorkerInstance,
//     which satisfies serverkit.ContextWorker, so the supervisor's
//     `w.(serverkit.ContextWorker)` assertion sees through the wrapper
//     and uses the ctx-aware lifecycle;
//   - otherwise → *WorkerInstance, which has no RunContext method, so
//     the supervisor's legacy Start path is untouched.
func WrapWorker(name string, w WorkerLifecycle) serverkit.Worker {
	if cr, ok := w.(contextRunner); ok {
		return &ContextWorkerInstance{
			WorkerInstance: WorkerInstance{name: name, start: w.Start, stop: w.Stop},
			runContext:     cr.RunContext,
		}
	}
	return NewWorkerInstance(name, w.Start, w.Stop)
}
