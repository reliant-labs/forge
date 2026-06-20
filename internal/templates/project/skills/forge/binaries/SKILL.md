---
name: binaries
description: Non-server long-running binaries — when to use `forge add binary` vs `forge add worker` vs `forge add service`, plus the lifecycle and deploy story.
---

# Binaries

A "binary" is a non-server long-running process that ships its own Deployment. Examples: a reverse proxy, a workspace gateway, an off-service NATS consumer, an authentication sidecar. Forge scaffolds binaries via `forge add binary <name>`.

A binary is **not** a forked `main()`. It is a real, owned cobra subcommand with its own typed composition root, running under the *same* serverkit lifecycle as the server — so it never hand-rolls OTel, signal handling, healthz/readyz, metrics, or graceful shutdown. serverkit owns the genuinely-uniform lifecycle; the binary owns *which* handler/workers it composes.

Binaries sit alongside services, workers, and operators as the four long-running shapes forge knows how to scaffold. The decision tree below shows when to pick which.

## When to use a binary vs a worker vs a service

| Shape | What it is | Picks this when |
|---|---|---|
| **service** | Connect-RPC server mounted via its `MountX` into a composition root. | You're exposing typed RPCs to clients (frontend, other services, daemons). |
| **worker** | In-process goroutine supervised by serverkit. Constructed in the same `Build` as services, starts when the server boots. | You need background work that *belongs to* the server's lifecycle (queue consumer, periodic sweep, cron). One process, no separate Deployment. |
| **binary** | Standalone process with its **own** cobra subcommand and composition root, run under serverkit, with its own Deployment. | You need a process with its own deploy lifecycle: a reverse proxy, a workspace gateway, a webhook receiver isolated from the API server. Also covers **one-off operational binaries** — backfills, data migrations, ad-hoc scripts. |
| **operator** | Kubernetes controller-runtime manager with CRD reconcilers. | You're reconciling Kubernetes resources (CRDs). Operators are a specialised flavour of binary; pick this only when CRDs are involved. |

The litmus test for binary vs worker is: **does this need its own Deployment, OR is it a run-once operational tool?** If either — it's a binary. If neither — it's a worker.

Resist the temptation to drop a `cmd/<name>/main.go` with its own `package main` into the tree. A forked `main()` outside the shared cobra root re-implements ~200 lines serverkit already owns (OTel, signals, shutdown, healthz/readyz, metrics), is invisible to `forge generate`/`build`/`deploy`, doesn't get a typed composition root, and doesn't get an image tag. `forge add binary` folds it in as a peer subcommand instead.

## The cmd/ surface (real, owned subcommands)

The cobra surface is real, owned Go — one symbol per command you can jump to — not a string-projection of a registry.

- **`cmd/main.go`** (yours) — the cobra root.
- **`cmd/<svc>.go`** (one per server/service) — a real subcommand, typically a one-liner via the `serverCmd` helper: `serverCmd("billing", app.MountBilling)`. The multi-mount `server` subcommand composes every `MountX`.
- **`cmd/<binary>.go`** (yours, from `forge add binary`) — a real subcommand for the binary, also `serverCmd("<name>", buildFn)`, where `buildFn` is the binary's own composition root.

There is **no** `cmd/services_gen.go` string-projection, **no** `RegisteredServices` constructor table, and **no** `userCommands()` catch-all. Each command is `serverCmd(name, mountFn)` over a real composition root. A data-only registry (`{Name, ConnectPath, Mount}`) survives for introspection (`forge map`/`audit`, CLI listing) — names there are for display, never a construction lookup key.

## Adding a binary

```bash
forge add binary workspace-proxy
forge add binary auth-sidecar
```

This creates:

```
cmd/<package>.go                     # serverCmd("<name>", BuildWorkspaceProxy) — real subcommand
internal/<package>/build.go          # BuildWorkspaceProxy(infra) (http.Handler, error) — composition root
internal/<package>/<package>.go      # the handler/runner body
internal/<package>/<package>_test.go # composition + lifecycle tests
```

Plus an entry under `binaries:` in `forge.yaml` so deploy emits a Deployment:

```yaml
binaries:
  - name: workspace-proxy
    path: cmd/workspace_proxy.go
    kind: long-running
```

The hyphenated CLI name (e.g. `workspace-proxy`) becomes the Go package name with hyphens replaced by underscores (`workspace_proxy`), so `cmd/workspace_proxy.go` and `internal/workspace_proxy/` line up with `package workspace_proxy`.

## Binary composition root

A binary participates in the composition model exactly like the server: it has its **own typed `Build`** that constructs its dependency closure in topological order, filling each component's `Deps` as interface-typed fields **resolved by type, never by string name**. It shares the same `buildShared(infra)` factory the server uses for infra and in-process services.

```go
// internal/workspace_proxy/build.go
func BuildWorkspaceProxy(infra Infra) (http.Handler, error) {
    shared := buildShared(infra)            // same infra/services the server root reuses
    enf := enforcement.New(shared.Cfg.Enforcement)  // per-binary singleton — one var per Build
    return newProxyHandler(proxyDeps{
        Enforcement: enf,                   // interface seam: real in-proc here,
        Users:       shared.Users,          // a Connect client if split out later
        Cfg:         shared.Cfg.Proxy,      // scalar config is a typed Cfg field
    })
}
```

Notes:
- **In-process is the default.** A cross-binary collaborator is the one-line swap: fill its interface with a Connect client instead of the in-proc instance; the consumer is untouched.
- **Per-binary singletons are natural.** A collaborator that must be one instance within *each* process (e.g. `enforcement` in both the server and the workspace-proxy) is just one `var` per `Build`. `buildShared` factors what both roots reuse.
- **Two-phase wiring is first-class.** Construct, then inject via setters (`x.WithY(z)`) for near-diamonds — plain method calls after both ends exist.
- **Scalars are config, not collaborators.** A `string`/`int`/`bool`/`Duration` lives in a `<Component>Config` proto block consumed as one typed `Cfg config.<Component>Config` Deps field — never a naked scalar Deps field.

## Lifecycle — same serverkit as the server

The `serverCmd` helper runs the binary's composed handler under `serverkit.Run`, which owns OTel, signal handling, healthz/readyz, metrics, worker supervision, and graceful shutdown ordering. The binary supplies a composed `serverkit.Server`; it never re-rolls any of that.

```go
type Server struct {
    Handler    http.Handler   // mux with everything already mounted
    Workers    []Worker
    Operators  []Operator
    OnShutdown func(context.Context) error
}
func Run(ctx context.Context, cfg Config, srv Server) error   // NO args, NO names
```

For a binary that is purely a handler (proxy, gateway), `Build` returns the `http.Handler` and `serverCmd` wraps it as `Server{Handler: h}`. For one that runs loops, return workers in `Server.Workers` and let serverkit supervise them — don't hand-roll the goroutine/`<-ctx.Done()` dance.

### Interceptor ordering

If your binary mounts Connect handlers, the interceptor chain (otel-outermost → rate-limit → auth → audit) is composed **explicitly** in the handler assembly inside `Build` — never implied by registration order. Preserve it.

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

A binary's body is a composed `http.Handler` or a `serverkit.Worker` — not a hand-rolled server loop. serverkit runs `ListenAndServe`/shutdown for handlers and supervises workers. The shapes you compose:

### Reverse proxy / gateway

`Build` returns the proxy `http.Handler`; `serverCmd` mounts it under serverkit (which owns listen + graceful shutdown). You write the handler, not the lifecycle.

### Off-service NATS consumer

Implement `serverkit.Worker` and return it in `Server.Workers`; serverkit starts it on boot and cancels its ctx on shutdown.

```go
func (w *consumer) Start(ctx context.Context) error {
    sub, err := w.nats.Subscribe("events.>", w.handle)
    if err != nil { return err }
    defer sub.Unsubscribe()
    <-ctx.Done()
    return nil
}
```

### Periodic ticker

Same — a `serverkit.Worker` with a `time.Ticker`, returned in `Server.Workers`. No separate goroutine supervision in your code.

## Testing

`internal/<binary>/<binary>_test.go` ships:

1. **Composition test** — calls `BuildX` against an infra fixture (with collaborators mocked through their interface seams) and asserts it returns a usable handler/server. Because every dep is an interface filled in one place, "spin up this binary with X mocked" is a few-line, one-call operation against `Build`.
2. **Lifecycle test** — ctx-cancel of the composed worker(s); keep it so shutdown regressions stay caught.

Binaries that don't mount RPCs don't use `tdd.RunRPCCases`; ones that do mount handlers test those the same way services do.

## Lint

`forge lint` / `forge map` check that:

- Every `binaries:` entry has a matching `cmd/<package>.go` and `internal/<package>/build.go`.
- The `Path` in forge.yaml matches the scaffolded `cmd/<package>.go`.
- Names don't collide with reserved cobra subcommands (`server`, `version`, `db`).
- The composition graph is acyclic and has no narrow-interface silent drops or unresolved non-optional deps.

Run `forge generate` after editing `binaries:` to keep the scaffold and config in lockstep.

## See also

- `forge skill load services` — Connect-RPC service shape, `MountX`, when to pick this instead.
- `forge skill load workers` — in-process workers supervised by serverkit.
- `forge skill load deploy` — KCL deploy story, including how binaries become Deployments.
- `forge skill load architecture` — composition roots (`Build`), `buildShared`, serverkit lifecycle, the interface seam — the wiring every binary participates in via its own `Build`.
