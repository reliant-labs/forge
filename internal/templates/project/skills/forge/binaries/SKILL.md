---
name: binaries
description: Non-server long-running binaries — when to use `forge add binary` vs `forge add worker` vs `forge add service`, plus the lifecycle and deploy story.
---

# Binaries

A "binary" is a non-server long-running process that ships its own Deployment. Examples: a reverse proxy, a workspace gateway, an off-service NATS consumer, an authentication sidecar. Forge scaffolds binaries via `forge add binary <name>`.

A binary is its own thin `package main` cobra root under `cmd/<package>/main.go` (devspace idiom — each binary gets its own `cmd/<bin>/` tree), delegating to a small owned composition root in `internal/<package>/`. It does NOT share the server's `internal/app/compose.go` `NewComponents` — that bag constructs the server's handlers/workers; a standalone binary owns its own narrow `Deps`/`Service`/`New` contract. The binary owns *which* runtime loop it composes; it reuses the same typed `internal/config` flag set as the server.

Binaries sit alongside services, workers, and operators as the four long-running shapes forge knows how to scaffold. The decision tree below shows when to pick which.

## When to use a binary vs a worker vs a service

| Shape | What it is | Picks this when |
|---|---|---|
| **service** | Connect-RPC server mounted via its `MountX` method onto the server's `serverkit.Server`. | You're exposing typed RPCs to clients (frontend, other services, daemons). |
| **worker** | In-process goroutine supervised by serverkit. Constructed in the same `internal/app/compose.go` `NewComponents` as services; `WorkerList(components)` adapts it and the cmd serve path starts it when the server boots. | You need background work that *belongs to* the server's lifecycle (queue consumer, periodic sweep, cron). One process, no separate Deployment. |
| **binary** | Standalone process with its **own** `cmd/<bin>/main.go` cobra root and its own owned `Deps`/`Service`/`New` composition root in `internal/<package>/`, with its own Deployment. | You need a process with its own deploy lifecycle: a reverse proxy, a workspace gateway, a webhook receiver isolated from the API server. Also covers **one-off operational binaries** — backfills, data migrations, ad-hoc scripts. |
| **operator** | Kubernetes controller-runtime manager with CRD reconcilers. | You're reconciling Kubernetes resources (CRDs). Operators are a specialised flavour of binary; pick this only when CRDs are involved. |

The litmus test for binary vs worker is: **does this need its own Deployment, OR is it a run-once operational tool?** If either — it's a binary. If neither — it's a worker.

Resist the temptation to hand-roll a `cmd/<name>/main.go` from scratch. `forge add binary` scaffolds the `cmd/<package>/main.go` entry point AND the `internal/<package>/` composition root for you, registers it in `forge.yaml` under `binaries:`, and gives it an image tag — so it stays visible to `forge generate`/`build`/`deploy`. A from-scratch `main()` that doesn't go through `forge add binary` re-implements config/flag loading and shutdown plumbing the scaffold already solves, and is invisible to deploy.

## The cmd/ surface

The server binary's cobra surface is real, owned Go — one symbol per command you can jump to — not a string-projection of a registry.

- **`cmd/<server>/main.go`** (yours) — the server cobra root with the full command tree.
- The server's `serve` command applies a typed `(*app.Components).Mount<Svc>` method expression (or `MountAll`) onto a `serverkit.Server`; named subsets compose the typed `app.MountByName` function values.
- **`cmd/<binary>/main.go`** (yours, from `forge add binary`) — a separate thin `package main` cobra root for each standalone binary, NOT a subcommand under the server root. It loads config, constructs the binary's `Service` via `internal/<package>.New(Deps{...})`, and calls `svc.Run(ctx)`.

There is **no** `cmd/services_gen.go` string-projection, **no** `RegisteredServices` constructor table, and **no** `userCommands()` catch-all. A data-only `Inventory` registry survives for introspection (`forge map`/`audit`, CLI listing) — names there are for display, never a construction lookup key.

## Adding a binary

```bash
forge add binary workspace-proxy
forge add binary auth-sidecar
```

This creates:

```
cmd/<package>/main.go                # thin package main cobra root — load config, New(Deps), svc.Run(ctx)
internal/<package>/contract.go       # Deps + Service interface + New(Deps) (Service, error) + validateDeps
internal/<package>/<package>.go      # Runner struct + Run(ctx) runtime body
internal/<package>/<package>_test.go # construction + lifecycle tests
```

Plus an entry under `binaries:` in `forge.yaml` so deploy emits a Deployment:

```yaml
binaries:
  - name: workspace-proxy
    path: cmd/workspace_proxy/main.go
    kind: long-running
```

The hyphenated CLI name (e.g. `workspace-proxy`) becomes the Go package name with hyphens replaced by underscores (`workspace_proxy`), so `cmd/workspace_proxy/main.go` and `internal/workspace_proxy/` line up with `package workspace_proxy`.

## Binary composition root

A binary owns its **own small typed composition root** in `internal/<package>/contract.go`: a `Deps` struct of interface-typed fields **resolved by type, never by string name**, gated by `validateDeps()` at construction, and a `New(Deps) (Service, error)` constructor. The thin `cmd/<package>/main.go` builds the `Deps` (logger, typed config, plus any DB/NATS/k8s/HTTP collaborators) and hands them to `New`. This is the binary's analogue of the server's `internal/app/compose.go` `NewComponents` — but local to the binary, not the server's `Components` bag.

```go
// internal/workspace_proxy/contract.go
type Deps struct {
    Logger *slog.Logger
    Config *config.Config
    // Add binary-specific deps below, each as a narrow interface:
    Enforcement Enforcer    // interface seam: real in-proc here, a Connect client if split out later
    Users       UserSource  // a local interface, not the concrete type
}

func New(deps Deps) (Service, error) {
    if err := deps.validateDeps(); err != nil {
        return nil, err
    }
    return &Runner{deps: deps}, nil
}
```

Notes:
- **In-process is the default.** A cross-binary collaborator is the one-line swap: fill its interface with a Connect client instead of the in-proc instance; the consumer is untouched.
- **Per-binary singletons are natural.** A collaborator that must be one instance within *each* process is constructed once in `cmd/<package>/main.go` and passed into `Deps`.
- **Two-phase wiring is first-class.** Construct, then inject via setters (`x.WithY(z)`) for near-diamonds — plain method calls after both ends exist, in `main.go`.
- **Scalars are config, not collaborators.** A `string`/`int`/`bool`/`Duration` lives in a `<Component>Config` proto block consumed as one typed `Cfg config.<Component>Config` Deps field — never a naked scalar Deps field.

## Lifecycle — Run blocks until ctx is cancelled

The generated `cmd/<package>/main.go` wires `SIGINT`/`SIGTERM` into a context via `signal.NotifyContext`, constructs the binary's `Service` via `New(Deps{...})`, and calls `svc.Run(ctx)`. The binary's `Runner.Run(ctx)` blocks until ctx is cancelled, then returns cleanly. Graceful shutdown is the binary's own responsibility *inside* `Run` (close servers, drain consumers) — the cobra adapter just propagates cancellation.

```go
// internal/<package>/contract.go
type Service interface {
    Run(ctx context.Context) error // blocks until ctx cancel or fatal error
    Name() string
}
```

For a binary that is purely an HTTP server (proxy, gateway), `Run` starts `srv.ListenAndServe()` in a goroutine and calls `srv.Shutdown` on `<-ctx.Done()`. For a loop (NATS consumer, ticker), `Run` selects on `ctx.Done()` directly. The scaffold's placeholder `Run` body blocks on `<-ctx.Done()` so a freshly added binary compiles and exits cleanly on Ctrl-C.

### Interceptor ordering

If your binary mounts Connect handlers, the interceptor chain (otel-outermost → rate-limit → auth → audit) is composed **explicitly** in the handler assembly inside `Run` — never implied by registration order. Preserve it.

### One-off scripts (backfills, migrations, ad-hoc tools)

A one-off binary's work-loop is allowed to finish: do the work, log the summary, return `nil`. Deploy it as a `forge.Job` instead of a `forge.Service` so Kubernetes won't restart it. Same scaffold, same composition root; the only difference is the KCL block (`forge.Job` vs `forge.Service`) in `deploy/kcl/<env>/main.k`.

## Config — the cmdkit paved path

Binaries do **not** hand-roll `os.Getenv`, ad-hoc `slog.Logger`s, hardcoded timeouts, or hand-rolled shutdown. The single typed `internal/config` — generated from the proto config blocks (`AppConfig` + `<Component>Config` with `(forge.v1.config)` annotations) — serves server, CLI, and standalone-binary kinds alike via the **cmdkit paved path**: DB open, logger, flag/env binding, and report envelope, with the logger injected through `observe.WithLogger`/`FromContext`.

Per-env config (logging, env vars) lives directly in `deploy/kcl/<env>/` — not in a redundant second YAML. `forge.yaml` stays strictly top-level (project identity, features, deploy provider, the `binaries:` list).

## Errors

If your binary mounts handlers, every handler maps errors with `svcerr.Wrap(err)` from `forge/pkg/svcerr` — never a raw `connect.NewError(connect.CodeInternal, err)` (it flattens NotFound/InvalidArgument/PermissionDenied) and never a hand-rolled per-binary `mapServiceError`/`toConnectError`. Domain failures use `svcerr` sentinels (`svcerr.NotFound("workspace")`). Skip per-RPC `if deps.X == nil` checks for non-optional deps — `validateDeps()` gates them at construction.

## Deploy

The `binaries:` block seeds `deploy/kcl/<env>/main.k` with a `forge.Service { name = "<bin>", command = ["./<bin>", "<name>"], ... }` — the same `forge.Service` schema services use, just a different `command`:

```kcl
# deploy/kcl/prod/main.k
import forge

_prod_k8s = forge.K8sCluster {
    cluster = "gke_acme-prod_us-central1_c1"
    namespace = "myapp-prod"
    registry = "ghcr.io/acme/myapp"
}

_bundle = forge.Bundle {
    services = [
        forge.Service { name = "api", deploy = _prod_k8s }
        forge.Service {
            name = "workspace-proxy"
            command = ["./myapp", "workspace-proxy"]
            deploy = _prod_k8s | { replicas = 2 }
        }
    ]
}
```

Binaries can target any deploy provider (K8sCluster / External / Compose / HostDeploy / BuildOnly) — same dispatch as services. For an Ingress, extra env vars, or per-binary resources, set them on the `forge.Service` block (`ingress`, `env_vars`, `resources` on `K8sCluster`).

## Common patterns

A binary's body is `Runner.Run(ctx)` — the placeholder blocks on `<-ctx.Done()`; you replace it with one of these shapes. `Run` owns its own listen/shutdown or loop, propagating cancellation from the ctx the cobra adapter wires SIGINT/SIGTERM into.

### Reverse proxy / gateway

`Run` starts the proxy `http.Server` in a goroutine and calls `srv.Shutdown` on `<-ctx.Done()`. You write the handler and the listen/shutdown dance against the cancelled ctx.

### Off-service NATS consumer

`Run` subscribes, then blocks until ctx cancellation, unsubscribing on the way out:

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

`Run` selects on a `time.Ticker` and `ctx.Done()` — a plain loop in the binary's own `Run`, no external supervisor.

## Testing

`internal/<binary>/<binary>_test.go` ships:

1. **Construction test** — calls `New(Deps{...})` with collaborators mocked through their interface seams, and asserts `validateDeps` accepts a complete `Deps` and rejects missing required fields. Because every dep is an interface filled in one place, "spin up this binary with X mocked" is a few-line, one-call operation against `New`.
2. **Lifecycle test** — start `Run(ctx)` in a goroutine, cancel ctx, assert it returns cleanly; keep it so shutdown regressions stay caught.

Binaries that don't mount RPCs don't use `tdd.RunRPCCases`; ones that do mount handlers test those the same way services do.

## Lint

`forge lint` / `forge map` check that:

- Every `binaries:` entry has a matching `cmd/<package>/main.go` and `internal/<package>/contract.go`.
- The `Path` in forge.yaml matches the scaffolded `cmd/<package>/main.go`.
- Names don't collide with reserved cobra subcommands (`server`, `version`, `db`).
- The composition graph is acyclic and has no narrow-interface silent drops or unresolved non-optional deps.

Run `forge generate` after editing `binaries:` to keep the scaffold and config in lockstep.

## See also

- `forge skill load services` — Connect-RPC service shape, `MountX`, when to pick this instead.
- `forge skill load workers` — in-process workers supervised by serverkit.
- `forge skill load deploy` — KCL deploy story, including how binaries become Deployments.
- `forge skill load architecture` — the server's explicit composition (`internal/app/providers.go` `Infra`/`OpenInfra` + `internal/app/compose.go` `NewComponents`), serverkit lifecycle, the interface seam — the wiring the server participates in; a standalone binary owns its own narrow `Deps`/`Service`/`New` analogue.
