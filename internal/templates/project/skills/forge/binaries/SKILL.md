---
name: binaries
description: Non-server long-running binaries — when to use `forge add binary` vs `forge add worker` vs `forge add service`, plus the lifecycle and deploy story.
---

# Binaries

A "binary" is a non-server long-running process that ships its own Deployment. Examples: a reverse proxy, a sidecar, an off-service NATS consumer, an authentication gateway. Forge scaffolds binaries via `forge add binary <name>`.

Binaries sit alongside services, workers, and operators as the four long-running shapes forge knows how to scaffold. The decision tree below shows when to pick which.

## When to use a binary vs a worker vs a service

| Shape | What it is | Picks this when |
|---|---|---|
| **service** | Connect-RPC server registered with `pkg/app/bootstrap.go`. | You're exposing typed RPCs to clients (frontend, other services, daemons). |
| **worker** | In-process goroutine running under the canonical server lifecycle. Gets the same Deps as services and starts when the server boots. | You need background work that *belongs to* the server's lifecycle (queue consumer, periodic sweep, cron). One process, no separate Deployment. |
| **binary** | Standalone long-running process with its own cobra subcommand and Deployment. Does not register Connect handlers. | You need a process with its own deploy lifecycle: a reverse proxy, a sidecar, a webhook receiver isolated from the API server, a tools daemon. |
| **operator** | Kubernetes controller-runtime manager with CRD reconcilers. | You're reconciling Kubernetes resources (CRDs). Operators are a specialised flavour of binary; pick this only when CRDs are involved. |

The litmus test for binary vs worker is: **does this need its own Deployment?** If yes — different scaling, different crash blast radius, different image — it's a binary. If no, it's a worker.

## Adding a binary

```bash
forge add binary workspace-proxy
forge add binary auth-sidecar --kind long-running
```

This creates four files:

```
cmd/<package>.go                    # cobra subcommand (registered against the shared root)
internal/<package>/contract.go       # Deps, Service, New(deps) (*Runner, error)
internal/<package>/<package>.go      # Runner.Run(ctx) lifecycle body
internal/<package>/<package>_test.go # lifecycle + validateDeps tests
```

Plus an entry under `binaries:` in `forge.yaml` so deploy emits a Deployment for the binary:

```yaml
binaries:
  - name: workspace-proxy
    path: cmd/workspace_proxy.go
    kind: long-running
```

The hyphenated CLI name (e.g. `workspace-proxy`) becomes the Go package name with hyphens replaced by underscores (`workspace_proxy`), so `cmd/workspace_proxy.go` and `internal/workspace_proxy/` line up with `package workspace_proxy`.

## Binary lifecycle

Every binary follows the same Deps + validateDeps + New shape forge uses for services and packages:

```go
// internal/workspace_proxy/contract.go
type Deps struct {
    Logger *slog.Logger
    Config *config.Config
    // Add binary-specific deps here.
}

func (d Deps) validateDeps() error { /* check required fields */ }

type Service interface {
    Run(ctx context.Context) error
    Name() string
}

func New(deps Deps) (*Runner, error) {
    if err := deps.validateDeps(); err != nil { return nil, err }
    return &Runner{deps: deps}, nil
}
```

The `cmd/<package>.go` cobra subcommand is the thin adapter: it loads config, builds Deps, calls `New(deps).Run(ctx)`. SIGINT/SIGTERM is wired into ctx so `Run` can drain in-flight work.

`Runner.Run(ctx)` is where you put the actual loop — `http.Server.ListenAndServe`, NATS consumer, ticker. The scaffolded body blocks on `<-ctx.Done()` so the freshly-generated binary builds and exits cleanly on Ctrl-C; replace it.

## Sharing the canonical server's flag set

The scaffolded `cmd/<package>.go` reuses the canonical `serverCmd.Flags()` so the same env-driven `pkg/config.Config` (DATABASE_URL, NATS, log level, OTel endpoint) is available without re-implementing flag parsing:

```go
func init() {
    {{`{{.Package}}`}}Cmd.Flags().AddFlagSet(serverCmd.Flags())
    rootCmd.AddCommand({{`{{.Package}}`}}Cmd)
}
```

Drop this line if your binary needs a strict subset of flags. Add binary-specific flags below it.

## Deploy

The `binaries:` block in forge.yaml is consumed by the KCL deploy templates: each entry produces a Deployment that runs the cobra subcommand (`./<bin> <name>`). The Deployment shape mirrors `cmd/server.go`'s — same image, different command — so your binaries inherit the project's image-pull, registry, and resource-limit defaults without re-stating them.

If a binary needs an Ingress, port forwarding, or extra env vars beyond the shared `pkg/config` set, hand-edit `deploy/kcl/<env>/main.k` to extend the binary's Deployment after `forge generate`.

## Common patterns

### Reverse proxy

```go
func (r *Runner) Run(ctx context.Context) error {
    srv := &http.Server{Addr: ":" + r.deps.Config.Port, Handler: r.proxyHandler}
    errCh := make(chan error, 1)
    go func() { errCh <- srv.ListenAndServe() }()
    select {
    case <-ctx.Done():
        shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        return srv.Shutdown(shutdownCtx)
    case err := <-errCh:
        return err
    }
}
```

### Off-service NATS consumer

```go
func (r *Runner) Run(ctx context.Context) error {
    sub, err := r.deps.NATS.Subscribe("events.>", r.handle)
    if err != nil { return err }
    defer sub.Unsubscribe()
    <-ctx.Done()
    return nil
}
```

### Periodic ticker

```go
func (r *Runner) Run(ctx context.Context) error {
    t := time.NewTicker(r.interval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return nil
        case <-t.C:
            r.tick(ctx)
        }
    }
}
```

## Testing

`internal/<binary>/<binary>_test.go` ships two tests by default:

1. **TestRunnerStartStop** — ctx-cancel lifecycle. Covers the "binary blocks until ctx cancellation, then exits cleanly" guarantee. Keep this case when you replace `Run`'s body so shutdown regressions stay caught.
2. **TestNewValidatesDeps** — table-driven validateDeps. Add cases here as you grow Deps with required fields.

Binaries don't have RPCs, so `tdd.RunRPCCases` doesn't apply. Use plain table-driven tests or hand-rolled fakes for the dependencies your `Run` body consumes.

## Lint

`forge lint` checks that:

- Every `binaries:` entry has a matching `cmd/<package>.go` and `internal/<package>/contract.go`.
- The `Path` field in forge.yaml matches the directory the scaffolder produces (`cmd/<package>.go`).
- Names don't collide with reserved cobra subcommands (`server`, `version`, `db`).

Run `forge generate` after editing `binaries:` in forge.yaml to keep the scaffold and config in lockstep.

## See also

- `forge skill load services` — Connect-RPC service shape, when to pick this instead.
- `forge skill load workers` — in-process background loops sharing the server lifecycle.
- `forge skill load deploy` — KCL deploy story, including how binaries become Deployments.
- `forge skill load architecture` — pkg/app/bootstrap.go, wire_gen, AppExtras — the wiring binaries don't participate in (they have their own thin entry point).
