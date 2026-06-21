---
name: v0.x-to-serverkit-composed
description: Migrate serverkit from Run(ctx, cfg, hooks, args) — where args were service NAMES matched through a string registry — to Run(ctx, cfg, serverkit.Server{Handler, Logger, Workers, Operators, RunOperators, OnShutdown, ...}). The Application interface, Hooks, and string-keyed BootstrapOnly selection are gone; serverkit takes an already-composed Server. Service/worker/operator SELECTION moves UP into cmd/server.go, which builds the mux + interceptor chain itself and selects a subset by name over the data-only app.Inventory (names = display/selection only, never construction keys). Use when bumping across the composed-server release.
relevance: migration
---

# Migrating to the composed-`Server` serverkit entrypoint

Use this skill when `forge upgrade` reports a jump across the release that
replaces serverkit's string-driven `Run(ctx, cfg, hooks, args)` with a
handler-shaped `Run(ctx, cfg, serverkit.Server{...})`. This is the second,
deeper serverkit change — `migrations/v0.x-to-serverkit` extracted the
lifecycle into the library; this one removes string-keyed *selection* from the
entrypoint entirely.

If your project is still on the pre-serverkit inline `cmd/server.go`, run
`migrations/v0.x-to-serverkit` first, then this skill.

## 1. What changed

**Before.** serverkit took service *names* and pushed the "which services"
decision down into the DI graph:

```go
// cmd/server.go — old
serverkit.Run(ctx, cfg, hooks, args)   // args == []string{"billing", ...}

// hooks.Bootstrap(ctx, mux, logger, names) → app.BootstrapOnly(names)
//   → appkit.Options{Only: names} → string-match mount in appkit.Run
```

The generated `cmd/services_gen.go` projected a registry string table into fake
cobra wrappers (`RunE: runServer(cmd, []string{"billing"})`). Selection was
welded into the framework entrypoint *and* the DI graph.

**After.** serverkit runs a *composed* server and owns no selection — it takes
an already-built handler plus the selected workers/operators:

```go
// forge/pkg/serverkit
type Server struct {
    Handler      http.Handler                  // mux with everything already mounted
    Logger       *slog.Logger
    Workers      []Worker                       // already-selected
    Operators    []Operator                     // already-selected
    RunOperators func(ctx context.Context, logger *slog.Logger, healthProbeAddr string) error
    OnShutdown   func(context.Context) error
    // + project edge-middleware factories (CORS / SecurityHeaders / RequestID)
    // and the same lifecycle config knobs (in Config) as before.
}
func Run(ctx context.Context, cfg Config, srv Server) error   // no hooks, no args, no names
```

Gone entirely:

- `serverkit.Application` interface and the `Hooks` struct (Bootstrap /
  BootstrapOnly dispatch by name).
- The `args []string` / `names` parameter on `Run`.
- `cmd/services_gen.go` string-projection wrappers and the `appkit`
  `Options.Only` string filter. (The `appkit` package still exists for
  worker-wrapping — `appkit.WrapWorker` — but its selection filter is gone.)

Selection moves **up into the cmd layer** — but it stays a *single* `server`
command, not one composition-root subcommand per service. The generated
`cmd/server.go` now owns the whole composition:

- builds the mux, mounts `/metrics`, runs migrations, and constructs the
  Connect interceptor chain itself (the work `Hooks.Bootstrap` used to do);
- runs the DI (`app.OpenInfra → app.Build → app.PostBuild` — see the typed-DI
  migration) to get the constructed `*Services`;
- **selects which services to mount by name over the data-only
  `app.Inventory`** (`internal/app/inventory_gen.go`). `runServer(cmd, args)`
  takes optional positional service names; an empty slice mounts every
  inventory row, a non-empty slice mounts only the named subset. Names are
  **display/selection only — never construction keys**: an excluded service is
  still *constructed* (cross-service reads stay nil-safe), only its routes are
  skipped.
- selects workers/operators the same way (`selectWorkers` / `selectOperators`
  over `app.WorkerList` / `app.OperatorList`), replicating serverkit's old
  per-name gating, and packs the finished handler + selections into
  `serverkit.Server`.

```go
// cmd/server.go (GENERATED) — one command; selection is positional args.
//   server                  → mount all services + run all workers/operators
//   server billing audit    → mount only those; others constructed, not mounted
var serverCmd = &cobra.Command{Use: "server [services...]", RunE: runServer}
```

The forked `cmd/workspace-proxy/main.go` outlier folds away too: with a
handler-shaped serverkit it builds its own handler and passes a
`serverkit.Server` like everyone else (~200 lines of duplicated lifecycle
deleted).

## 2. Detection

```bash
# Old shape — name/hooks-driven Run.
grep -q "serverkit.Run(ctx, cfg, hooks" cmd/server.go && echo "OLD SHAPE — hooks+args Run"
grep -rq "BootstrapOnly" pkg/app/ cmd/ && echo "OLD SHAPE — string selection"
test -f cmd/services_gen.go && echo "OLD SHAPE — string-projection subcommands"

# New shape — composed Server + data-only inventory.
grep -rq "serverkit.Server{" cmd/ && echo "already on composed Server"
test -f internal/app/inventory_gen.go && echo "data-only mount inventory present"
```

## 3. Migration (deterministic part)

```bash
# 1. Bump forge_version in forge.yaml to the composed-server release.
# 2. Regenerate. The generator emits the composed cmd/server.go (owns the mux,
#    interceptor chain, DI, and name-based mount selection over app.Inventory),
#    emits internal/app/inventory_gen.go (the data-only mount table), and
#    deletes cmd/services_gen.go.
forge generate

# 3. Build.
go build ./...
```

For stock projects (un-forked `cmd/`) the regen is the whole migration — the
new `cmd/server.go` composes `serverkit.Server` for you.

## 4. Migration (manual part — forked cmd/server.go)

The painful case is a **disowned `cmd/server.go`** (control-plane is the
canonical example — a forked composition root plus the hand-rolled
`cmd/workspace-proxy/main.go`). `forge generate` leaves disowned files alone;
apply the shape change by hand, mirroring the generated `cmd/server.go`:

1. **Build the mux yourself, then pass it as `Server.Handler`.** Replace the
   old hooks dispatch with explicit mounting. Whatever `Hooks.Bootstrap` used
   to do — build the mux, mount services, attach interceptors — now happens in
   the cmd layer and the result is a plain `http.Handler`:

   ```go
   // before
   serverkit.Run(ctx, cfg, hooks, args)

   // after — build mux + interceptors, run DI, mount the selected subset.
   mux := http.NewServeMux()
   // ... /metrics, migrations, interceptor chain ...
   infra, _   := app.OpenInfra(ctx, cfg, logger)
   services, _ := app.Build(infra)
   _ = app.PostBuild(services)
   mounted := mountServices(services, mux, cfg, logger, args, opts...) // selection by name
   serverkit.Run(ctx, skCfg, serverkit.Server{
       Handler:   mux,        // or a REST transcoder wrapping it
       Logger:    logger,
       Workers:   selectWorkers(app.WorkerList(services), args),
       Operators: selectOperators(app.OperatorList(services), args),
       OnShutdown: shutdownFn,
   })
   ```

2. **Move selection out of the DI graph into cmd, over the data-only
   inventory.** Anywhere you relied on `BootstrapOnly(names)` / `Options.Only`,
   replace it with a `mountServices(...)` loop over `app.Inventory` that mounts
   only the named rows (empty `args` = all). Construction already happened in
   `app.Build`; selection only decides which rows get their routes registered.
   Delete the name-matching construction path.

3. **Map old hooks onto the composed inputs:**

   | Old hook / mechanism                | New home                                              |
   | ----------------------------------- | ----------------------------------------------------- |
   | `Hooks.Bootstrap` / `BootstrapOnly` | cmd builds `Server.Handler`; selects over `app.Inventory` |
   | `Hooks.PostBootstrap`               | call it inline after building the handler             |
   | worker / operator selection         | `selectWorkers` / `selectOperators` → `Server.Workers` / `.Operators` |
   | operator manager entry point        | `Server.RunOperators` (→ `app.RunOperators(services, …)`) |
   | graceful-shutdown hook              | `Server.OnShutdown`                                   |
   | `SetupOTel`, `AutoMigrate`          | cmd owns OTel + migrate; `Config` knobs unchanged    |

4. **Preserve interceptor ordering explicitly.** The old path prepended the
   canonical `observe.DefaultMiddlewares` chain and ran the project
   interceptors after it (otelconnect → rate-limit → auth → audit). With the
   handler built in cmd, *you* now own that ordering when constructing the
   interceptor chain and threading it as `HandlerOption`s into the mount — keep
   it identical. This is the easiest thing to silently regress.

5. **Fold `cmd/workspace-proxy/main.go` into a subcommand.** Replace the
   forked `main()` (OTel setup, signal handling, healthz/readyz, metrics
   server, shutdown — all of which serverkit owns) with a cobra subcommand that
   builds the proxy's own mux/deps and hands serverkit a `serverkit.Server`.
   The only thing that stays is the proxy's own handler-building composition
   root; everything else deletes.

## 5. Verification

```bash
go build ./... && go test ./... && forge lint
```

Shape checks:

```bash
grep -rq "serverkit.Server{" cmd/ && echo "composed Server in use"
! test -f cmd/services_gen.go && echo "string-projection subcommands gone"
! grep -rq "BootstrapOnly\|Options{Only" pkg/app/ cmd/ && echo "string selection gone"
test -f internal/app/inventory_gen.go && echo "data-only mount inventory present"

# Name selection is positional args on `server`, over app.Inventory.
go run . server billing audit    # mounts only billing + audit; others constructed, not mounted
go run . server                  # mounts everything
```

Smoke the lifecycle: start the `server` command, hit `/healthz` and `/readyz`,
send SIGTERM, confirm graceful drain — the behavior serverkit owns must be
unchanged.

## 6. Rollback

```bash
git revert <forge-generate-commit>       # undo the regen
git revert <cmd-rewrite-commit>          # undo the manual cmd/bootstrap edits
forge upgrade --to <prev-version>        # pin back to the prior version
```

`--to <prev-version>` requires the prior forge build on `PATH`
(`go install github.com/reliant-labs/forge/cmd/forge@vX.Y.Z`).

## See also

- `migrations/v0.x-to-serverkit` — the earlier lifecycle-extraction step; run
  it first if you're still on the inline `cmd/server.go`.
- `migrations/v0.x-to-typed-di` — the DI counterpart. Selection moving into
  cmd over a data-only inventory (this skill) and wiring moving to an owned
  by-type injector + `Infra`/`OpenInfra` (that skill) are two halves of the
  same redesign; do layout, then this, then DI.
- `binaries` skill — `forge add binary` / `forge add worker` and the
  per-binary composition-root pattern.
