---
name: v0.x-to-serverkit
description: Migrate the generated `cmd/server.go` from the ~520-line inline scaffold to a ~50-line shim over `forge/pkg/serverkit`. The library now owns the HTTP listener, observability chain, healthz/readyz, worker supervisor, operator manager, and graceful-shutdown sequence; the shim only projects config onto serverkit.Config and wires per-project hooks.
---

# Migrating to the `serverkit` runtime library

Use this skill when `forge upgrade` reports a jump across the version that
ships `forge/pkg/serverkit` (the release that drops the inline `cmd/server.go`
scaffold). It also covers the bootstrap.go shape change that comes with it.

## 1. What changed

The pre-serverkit `cmd/server.go` was a 500+ line scaffold emitted by
`internal/templates/project/cmd-server.go.tmpl`. Every project shipped the
same listener bind, signal handling, interceptor chain, healthz/readyz,
worker fan-out, operator gating, and shutdown sequence — all uniform code,
all hand-emitted. Tuning any of them meant editing the template (forge-side)
or hand-editing every generated file (user-side).

`forge/pkg/serverkit` extracts the uniform body into `serverkit.Run(ctx,
cfg, hooks, args)`. The new generated `cmd/server.go` is a ~50-line shim
that:

1. Loads + validates the project's typed `*config.Config`.
2. Projects it onto a vendor-neutral `serverkit.Config`.
3. Binds the per-project hooks (`Bootstrap`, `PostBootstrap`,
   `AutoMigrate`, `SetupOTel`, `ProjectInterceptors`, `CORSMiddleware`,
   `SecurityHeadersMiddleware`, `RequestIDMiddleware`).
4. Calls `serverkit.Run` and returns its error.

Bootstrap-side changes the regen also lands:

- `pkg/app/app_gen.go` — `RESTHandler` becomes a **method** (returning
  `http.Handler`) backed by an unexported `restHandler` field. Required by
  the `serverkit.Application` interface contract.
- `pkg/app/bootstrap.go` — `WorkerList()` returns `[]serverkit.Worker`
  (was `[]*WorkerInstance`); `OperatorList()` returns `[]serverkit.Operator`
  (was `[]*OperatorInstance`). `RunOperators` gains a third parameter
  `healthProbeAddr string` so projects that bind a controller-runtime
  health probe listener can forward `serverkit.Config.OperatorHealthProbeAddr`.

## 2. Detection

```bash
# Old shape — the bulky inline scaffold.
test -f cmd/server.go && wc -l cmd/server.go    # > 400 lines = pre-serverkit

# New shape — the serverkit shim.
grep -q "serverkit.Run" cmd/server.go && echo "already on serverkit"

# REST handler shape change.
grep -q "RESTHandler http.Handler" pkg/app/app_gen.go && echo "old field shape"
grep -q "func (a \*App) RESTHandler() http.Handler" pkg/app/app_gen.go && echo "new method shape"
```

## 3. Migration (deterministic part)

`forge generate` re-renders all three files (`cmd/server.go`,
`pkg/app/bootstrap.go`, `pkg/app/app_gen.go`). Most projects need no
hand-edits beyond the regen — the typed `*app.App` already satisfies
`serverkit.Application` thanks to the regenerated method shapes.

```bash
# 1. Bump forge_version in forge.yaml to the serverkit release.
$EDITOR forge.yaml

# 2. Regenerate.
forge generate

# 3. Build — should be clean.
go build ./...
```

## 4. Migration (manual part — forked bootstrap.go)

The painful case is **projects whose `pkg/app/bootstrap.go` is
forge-forked** (i.e. `.forge/checksums.json` shows the file as
user-modified or with `forked: true`). The control-plane reference
project (`cp-forge`) is the canonical example — it has a hand-rolled
`constructWorkers` helper and a custom `mountDaemonRegistryAdapter` that
the codegen wouldn't have produced.

For forked projects, `forge generate` will report the bootstrap.go
mismatch and skip the regen. You must apply the same shape changes by
hand:

1. **Add the serverkit import** to the import block:

   ```go
   "github.com/reliant-labs/forge/pkg/serverkit"
   ```

2. **Change `WorkerList` return type** from `[]*WorkerInstance` to
   `[]serverkit.Worker`. Wrap each `WorkerInstance` literal as
   `&WorkerInstance{...}` and let the interface satisfaction do the
   conversion — `WorkerInstance` already has `Name() / Start(ctx) /
   Stop(ctx)`.

3. **Change `OperatorList` return type** from `[]*OperatorInstance` to
   `[]serverkit.Operator`. Same wrap-as-pointer trick.

4. **Add `healthProbeAddr string` to `RunOperators`**:

   ```go
   func (a *App) RunOperators(ctx context.Context, logger *slog.Logger, healthProbeAddr string) error {
       // ...
       mgr, err := ctrl.NewManager(cfg, ctrl.Options{
           LeaderElection:         true,
           LeaderElectionID:       "<your-project>-leader",
           HealthProbeBindAddress: healthProbeAddr,    // <-- NEW
       })
       // ...
   }
   ```

   If your project previously read `HEALTH_PROBE_BIND_ADDRESS` from the
   environment inside `RunOperators`, switch to reading it in
   `cmd/server.go` and pass it via
   `serverkit.Config.OperatorHealthProbeAddr` instead — that keeps the
   binding decision in one place. (Falling back to the env var inside
   the hook still works; it just means the value isn't visible to
   anyone reading the projection.)

5. **Type-assert inside the OperatorList iteration in `RunOperators`**.
   Since `OperatorList` now returns `[]serverkit.Operator`, the
   `op.SetupWithManager(mgr)` call site needs a type assertion:

   ```go
   for _, op := range a.OperatorList() {
       inst, ok := op.(*OperatorInstance)
       if !ok {
           return fmt.Errorf("operator %q has unexpected type %T", op.Name(), op)
       }
       if err := inst.SetupWithManager(mgr); err != nil { ... }
   }
   ```

6. **Rename `app.RESTHandler` field reads** to `app.RESTHandler()` method
   calls in any hand-written code that reaches in (rare — the regen
   handles every codegen call site).

The `Bootstrap` / `BootstrapOnly` body — your typed service / worker /
operator loops, `constructWorkers`, `mountDaemonRegistryAdapter`, etc. —
stays untouched. The serverkit migration only touches the lifecycle
methods on `*App`.

## 5. Manual part — custom `cmd/server.go` edits

If your `cmd/server.go` was also forked (rare — most projects leave it
alone), the regen will skip it the same way. The canonical mapping from
the old inline scaffold to serverkit hooks:

| Old inline code                       | New hook                                  |
| ------------------------------------- | ----------------------------------------- |
| `setupOTel(ctx)` + metrics handler    | `Hooks.SetupOTel`                         |
| `app.Bootstrap` / `app.BootstrapOnly` | `Hooks.Bootstrap` (dispatches on `names`) |
| `app.PostBootstrap(app)`              | `Hooks.PostBootstrap`                     |
| `app.AutoMigrate(db, logger)`         | `Hooks.AutoMigrate` (DB owned by serverkit) |
| Project interceptor chain build       | `Hooks.ProjectInterceptors`               |
| `middleware.CORSMiddleware(...)`      | `Hooks.CORSMiddleware`                    |
| `middleware.SecurityHeadersMiddleware` | `Hooks.SecurityHeadersMiddleware`        |
| `middleware.RequestIDMiddleware`      | `Hooks.RequestIDMiddleware`               |
| pprof side-listener                   | `Config.PprofAddr` (serverkit owns)       |
| Worker supervisor goroutines          | serverkit owns — driven by `WorkerList()` |
| Operator manager goroutine + gating   | serverkit owns — driven by `HasOperators()` + `RunOperators(...)` |
| Graceful shutdown sequence            | serverkit owns — `Config.PreStopDelay`, `Config.ShutdownTimeout` |

The interceptor ordering convention is preserved: `serverkit.Run`
prepends the canonical `observe.DefaultMiddlewares` chain (recovery →
request-id → logging → tracing → metrics) and your `ProjectInterceptors`
slice runs AFTER it in supplied order (typical: otelconnect → rate-limit
→ auth → audit).

## 6. Verification

```bash
go build ./... && go test ./... && forge lint
```

Plus a quick sanity check on the shim shape:

```bash
grep "serverkit.Run" cmd/server.go    # should be present
wc -l cmd/server.go                    # should be ~150 lines or fewer
grep "func (a \*App) RESTHandler() http.Handler" pkg/app/app_gen.go
```

If all pass, `forge upgrade` will bump `forge_version` in `forge.yaml`
to the target version automatically.

## 7. Rollback

```bash
git revert <forge-generate-commit>      # undo the regen
forge upgrade --to <prev-version>        # pin back to the prior version
```

`--to <prev-version>` requires the prior forge build on `PATH`. Install
with `go install github.com/reliant-labs/forge/cmd/forge@v<X.Y.Z>`.

## See also

- `forge-libraries` skill — describes `pkg/serverkit` alongside
  `pkg/observe`, `pkg/testkit`, `pkg/crud`, etc.
- `architecture` skill — the generated-vs-hand-written split. Serverkit
  shrinks the "generated runtime" surface in favour of the "consumed
  library" model.
- `binaries` skill — `cmd/server.go` is still the canonical server
  entry; `forge add binary` / `forge add worker` haven't changed.
