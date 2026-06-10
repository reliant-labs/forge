---
name: workers
description: Background workers — adding, implementing, and testing workers (including cron-scheduled).
---

# Background Workers

Workers are long-running background processes that don't serve HTTP but participate in the single-binary lifecycle with the same dependency injection and graceful shutdown as services.

## Naming

Worker names canonicalize to lowercase with `-` and `_` stripped. The canonical form is what appears on disk, in the Go package decl, in `wire_gen.go` imports, and in the `forge.yaml` `path:` field. The display name in `forge.yaml` `name:` keeps its original spelling.

- `forge add worker calibrator_refit` → directory `workers/calibratorrefit/`, `package calibratorrefit`, `path: workers/calibratorrefit` in `forge.yaml`.
- `forge add worker email-sender` → directory `workers/emailsender/`, `package emailsender`, `path: workers/emailsender`.

**Migrating from a non-forge codebase:** if you have existing worker directories named `snake_case` or `kebab-case`, rename them to the canonical form *before* running `forge generate`. Otherwise `forge generate` will write `bootstrap.go` / `wire_gen.go` imports pointing at the canonical name (e.g. `workers/calibratorrefit`) while the code lives under the original (`workers/calibrator_refit/`), and the build will fail with missing-package errors. The canonical form in `forge.yaml` `services[].path:` is the source of truth — match the directory to it, not the other way around.

## Adding a Worker

```bash
forge add worker <name>
forge add worker <name> --kind cron --schedule "*/5 * * * *"
```

This creates:
- `workers/<name>/worker.go` — Worker implementation with `Start(ctx)` / `Stop(ctx)`
- `workers/<name>/worker_test.go` — Basic lifecycle test
- An entry in `forge.yaml` under `services:` with type `worker`

Run `forge generate` after adding a worker to wire it into `pkg/app/bootstrap.go`.

## Worker Lifecycle

Every worker implements the same contract:

```go
func (w *Worker) Name() string { return "email-sender" }

// Start runs the cycle loop until ctx is cancelled. The supervisor
// cancels ctx the moment graceful shutdown begins.
func (w *Worker) Start(ctx context.Context) error {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return nil // graceful shutdown — return promptly
        case <-ticker.C:
            if err := w.runOnce(ctx); err != nil {
                w.deps.Logger.Error("worker cycle failed", "error", err)
            }
        }
    }
}

// runOnce is a single work cycle. Pass ctx into every context-aware
// call so a long cycle observes shutdown mid-flight.
func (w *Worker) runOnce(ctx context.Context) error { /* ... */ return nil }

// Stop is called during graceful shutdown, after the loop returned.
func (w *Worker) Stop(ctx context.Context) error {
    w.deps.Logger.Info("worker stopping", "worker", w.Name())
    return nil
}
```

Key contract: `Start` must block until `ctx` is cancelled, then return promptly — the supervisor waits for it before continuing shutdown. `Stop` is called after cancellation with a deadline context for cleanup. Always thread `ctx` into per-cycle work (DB queries, HTTP calls, adapters) so in-flight cycles honor shutdown instead of running to completion.

A worker may also implement serverkit's optional `ContextWorker` extension — `RunContext(ctx context.Context) error` — which the supervisor prefers over `Start` when present (legacy `Start` workers are unaffected). Note: workers wired through the generated `pkg/app/bootstrap.go` are wrapped in `WorkerInstance`, which currently forwards only `Start`/`Stop`; for those, the ctx-aware `Start` loop above is the shutdown seam.

## Common Patterns

### Queue Consumer

```go
func (w *Worker) Start(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return nil
        default:
            msg, err := w.deps.Queue.Receive(ctx)
            if err != nil {
                time.Sleep(time.Second)
                continue
            }
            w.process(ctx, msg)
        }
    }
}
```

### Cron-Scheduled Worker

Use `--kind cron` with a `--schedule` (standard cron expression) to scaffold a worker that runs on a schedule using `robfig/cron/v3`:

```bash
forge add worker cleanup --kind cron --schedule "0 */6 * * *"
```

The generated worker has a `Run(ctx context.Context)` method for your job logic. The cron scheduler is managed inside `Start` and stopped on context cancellation — same lifecycle as a regular worker. The cron closure derives a per-tick `ctx` from a base context set in `Start` and cancelled in `Stop`, so long-running jobs can observe graceful shutdown via `ctx.Done()` instead of running to completion after `Stop` fires.

```go
func (w *Worker) Run(ctx context.Context) {
    // Your scheduled job logic here. Plumb ctx through every
    // context-aware downstream call so shutdown is observed.
    w.deps.Logger.InfoContext(ctx, "running scheduled cleanup")
}
```

> **MIGRATION (v0.x → v0.x+1):** `Run()` now takes a `context.Context`. If you've polished an existing `Run()` body, add a `ctx context.Context` parameter and thread it through any DB/HTTP/adapter call that already accepts a context. The scaffold's `Start` derives a per-tick ctx from `baseCtx`; `Stop` cancels `baseCtx` so in-flight ticks see `ctx.Done()` immediately rather than racing the cron `Stop()` wait group.

Cron workers are tracked with `kind: cron` and `schedule` in `forge.yaml`:
```yaml
services:
  - name: cleanup
    type: worker
    kind: cron
    schedule: "0 */6 * * *"
    path: workers/cleanup
```

### Simple Periodic (no cron)

For basic intervals without cron expressions, use a plain worker with a ticker:

```go
func (w *Worker) Start(ctx context.Context) error {
    ticker := time.NewTicker(5 * time.Minute)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return nil
        case <-ticker.C:
            w.runBatchJob(ctx)
        }
    }
}
```

## Adding Dependencies

Extend the `Deps` struct in the worker file, then wire them in `pkg/app/setup.go`:

```go
type Deps struct {
    Logger *slog.Logger
    Config *config.Config
    DB     *sql.DB
    Queue  *queue.Client
}
```

## Late-bound dependencies between workers

When worker A produces a value worker B needs (snapshot saver, registry, event sink), you can't put it in B's `Deps` — wire_gen resolves Deps once at construction and both workers are constructed in the same pass, so there's a construction-order cycle.

The seam is `PostBootstrap` in `pkg/app/post_bootstrap.go`, called after `Bootstrap` returns with the fully-constructed `*App`:

```go
func PostBootstrap(app *App) error {
    saver := app.Workers.Snapshotter.SnapshotSaver()
    app.Workers.Trader.SetSnapshotSaver(saver)
    return nil
}
```

`PostBootstrap` is user-owned; forge generate never overwrites it. An error returned here aborts boot loudly. **Don't invent a parallel hook system (`wire_*_hooks.go`, post-Setup passes) for this — PostBootstrap IS that system.** See the `interactor` skill for the full pattern.

## Testing

The generated test verifies basic start/stop lifecycle. For workers that process messages, inject a mock dependency and verify behavior:

```go
func TestWorkerProcessesMessage(t *testing.T) {
    mockQueue := &MockQueue{messages: []Message{{ID: "1", Body: "test"}}}
    w := New(Deps{Logger: slog.Default(), Config: &config.Config{}, Queue: mockQueue})

    ctx, cancel := context.WithCancel(context.Background())
    go w.Start(ctx)
    time.Sleep(100 * time.Millisecond)
    cancel()

    if mockQueue.processed != 1 {
        t.Fatalf("expected 1 processed message, got %d", mockQueue.processed)
    }
}
```

## Rules

- `Start()` must respect context cancellation — always select on `ctx.Done()`.
- `Stop()` receives a context with a deadline — finish cleanup before it expires.
- Worker names canonicalize to lowercase with `-` and `_` stripped — see the Naming section. On-disk directories must match the canonical form.
- Use `forge add worker`, not manual directory creation.
- `bootstrap.go` is regenerated — wire custom dependencies in `setup.go`.
- Cron workers require `--schedule` with a valid cron expression (5-field standard format).