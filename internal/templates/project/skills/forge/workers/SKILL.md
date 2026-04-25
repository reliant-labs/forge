---
name: workers
description: Background workers — adding, implementing, and testing workers.
---

# Background Workers

Workers are long-running background processes that don't serve HTTP but participate in the single-binary lifecycle with the same dependency injection and graceful shutdown as services.

## Adding a Worker

```bash
forge add worker <name>
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

// Start blocks until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) error {
    w.deps.Logger.Info("worker started", "worker", w.Name())
    <-ctx.Done()
    return nil
}

// Stop is called during graceful shutdown. Drain in-flight work here.
func (w *Worker) Stop(ctx context.Context) error {
    w.deps.Logger.Info("worker stopping", "worker", w.Name())
    return nil
}
```

Key contract: `Start` must block until `ctx` is cancelled. `Stop` is called after cancellation with a deadline context for cleanup.

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

### Periodic / Cron

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
- Worker names must be valid Go identifiers (lowercase, no hyphens).
- Use `forge add worker`, not manual directory creation.
- `bootstrap.go` is regenerated — wire custom dependencies in `setup.go`.
