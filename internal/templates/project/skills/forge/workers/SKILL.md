---
name: workers
description: Background workers — adding, implementing, and testing workers (including cron-scheduled).
---

# Background Workers

Workers are long-running background processes that don't serve HTTP but participate in the single-binary lifecycle with the same typed dependency injection and graceful shutdown as services. They live under `internal/workers/<name>/` — the `workers/` role subtree is nested under `internal/`, never a top-level `workers/` directory.

## Naming

Worker names canonicalize to lowercase **snake_case**: hyphens become underscores and PascalCase/camelCase boundaries split (`email-sender` → `email_sender`, `EmailSender` → `email_sender`, `calibrator_refit` stays `calibrator_refit`). The canonical form is what appears on disk, in the Go package decl, and in the `forge.yaml` `path:` field. The display name in `forge.yaml` `name:` keeps its original spelling.

- `forge add worker calibrator_refit` → directory `internal/workers/calibrator_refit/`, `package calibrator_refit`, `path: internal/workers/calibrator_refit` in `forge.yaml`.
- `forge add worker email-sender` → directory `internal/workers/email_sender/`, `package email_sender`, `path: internal/workers/email_sender`.

**Migrating from a non-forge codebase:** rename existing worker directories to the canonical snake_case leaf under `internal/workers/` *before* running `forge generate`. The `forge.yaml` `services[].path:` is the source of truth — match the directory to it, not the other way around.

## Adding a Worker

```bash
forge add worker <name>
forge add worker <name> --kind cron --schedule "*/5 * * * *"
```

This creates:
- `internal/workers/<name>/worker.go` — Worker implementation with `Start(ctx)` / `Stop(ctx)`
- `internal/workers/<name>/worker_test.go` — Basic lifecycle test
- An entry in `forge.yaml` under `services:` with type `worker`

After adding a worker, construct it in the explicit composition (`internal/app/compose.go` `NewComponents`, off the owned `internal/app/providers.go` `Infra`/`OpenInfra`) onto its `Components` field — see [Wiring](#wiring) below.

## Worker Lifecycle

Every worker implements the same contract:

```go
func (w *Worker) Name() string { return "email_sender" }

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

A worker may also implement serverkit's optional `ContextWorker` extension — `RunContext(ctx context.Context) error` — which the supervisor prefers over `Start` when present (legacy `Start` workers are unaffected).

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

Cron workers are tracked with `kind: cron` and `schedule` in `forge.yaml`:
```yaml
services:
  - name: cleanup
    type: worker
    kind: cron
    schedule: "0 */6 * * *"
    path: internal/workers/cleanup
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

## Wiring

A worker's `Deps` are interface-typed fields filled **by type in `internal/app/compose.go` `NewComponents`**, not by string-matched field names and not by a generated `wire_gen.go`. You construct the worker in type-topological order, pass it its collaborators' interfaces (read off the owned `infra.<Field>`), and assign it onto its `Components` field. The generated `internal/app/lifecycle.go` `WorkerList(c)` adapts every constructed worker, and the cmd serve path supervises them — you don't append to a slice here:

```go
type Deps struct {
    Logger *slog.Logger
    Cfg    config.CleanupConfig // scalars travel as one typed config block, not naked fields
    Queue  queue.Client         // an interface — the default fill is the real in-process impl
}

// in internal/app/compose.go
func NewComponents(infra *Infra) (*Components, error) {
    c := &Components{}
    q := queue.New(infra.QueueConn)          // construct collaborators once, off infra
    c.Cleanup = cleanup.New(cleanup.Deps{
        Logger: infra.Log,
        Cfg:    infra.Cfg.Cleanup,
        Queue:  q,                            // resolved by type/interface
    })
    return c, nil
}
```

Because `Queue` is an interface, swapping a mock (in a test) or a Connect client (when the producer moves to its own binary) is a one-line change in `NewComponents` with the worker untouched. There is no name-matched `*App` field, no typed-zero-with-TODO fallback, and no `wire_gen.go` — a collaborator that doesn't satisfy the interface fails to compile rather than being silently dropped.

## Late-bound dependencies between workers

When worker A produces a value worker B needs (snapshot saver, registry, event sink), you can't pass it through B's constructor — both workers are constructed in the same pass, so a constructor-only graph would deadlock. **Two-phase wiring is the answer, and it's just plain Go:** `forge disown internal/app/compose.go`, then construct both ends and inject with a setter, by hand inside `NewComponents`.

```go
func NewComponents(infra *Infra) (*Components, error) {
    c := &Components{}
    c.Snapshotter = snapshotter.New(...)
    c.Trader = trader.New(...)

    // construct-then-inject: both ends now exist
    c.Trader.SetSnapshotSaver(c.Snapshotter.SnapshotSaver())

    return c, nil
}
```

This is the canonical seam for near-diamonds and producer/consumer pairs. Don't invent a parallel hook system (`PostBootstrap`, `wire_*_hooks.go`, post-Setup passes) — disowning `compose.go` lets `NewComponents` support construct-then-inject directly. See the `interactor` skill for the full pattern.

## Testing

The generated test verifies basic start/stop lifecycle. For workers that process messages, inject a mock dependency and verify behavior:

```go
func TestWorkerProcessesMessage(t *testing.T) {
    mockQueue := &MockQueue{messages: []Message{{ID: "1", Body: "test"}}}
    w := New(Deps{Logger: slog.Default(), Queue: mockQueue})

    ctx, cancel := context.WithCancel(context.Background())
    go w.Start(ctx)
    time.Sleep(100 * time.Millisecond)
    cancel()

    if mockQueue.processed != 1 {
        t.Fatalf("expected 1 processed message, got %d", mockQueue.processed)
    }
}
```

Because `Deps` fields are interfaces filled in one place, instantiating a worker with mocked collaborators is a direct `New(Deps{...})` call — no framework, no string lookups, no globals.

## Rules

- `Start()` must respect context cancellation — always select on `ctx.Done()`.
- `Stop()` receives a context with a deadline — finish cleanup before it expires.
- Workers live under `internal/workers/<name>/`, never a top-level `workers/` dir. On-disk directory leaves must match the canonical snake_case form.
- Worker `Deps` are interface-typed and filled by type in `internal/app/compose.go` `NewComponents`; scalars travel in a typed `<Component>Config` block, never as naked Deps fields.
- Wire workers explicitly: construct in `NewComponents` onto the worker's `Components` field; the generated `lifecycle.go` `WorkerList` and the cmd serve path supervise them. For late-bound, cross-worker deps, `forge disown internal/app/compose.go` and use setters. There is no `wire_gen.go` and no name-matched `*App` resolution.
- Use `forge add worker`, not manual directory creation.
- Cron workers require `--schedule` with a valid cron expression (5-field standard format).
