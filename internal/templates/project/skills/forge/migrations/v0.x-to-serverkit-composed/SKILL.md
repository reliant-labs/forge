---
name: v0.x-to-serverkit-composed
description: Migrate serverkit from Run(ctx, cfg, hooks, args) — where args were service NAMES matched through a string registry — to Run(ctx, cfg, serverkit.Server{...}) with FULLY TYPED service selection. The Application interface, Hooks, string-keyed BootstrapOnly selection, the string→inventory mount lookup, AND the generated cmd/otel.go shim are all gone. The cmd layer becomes a real cobra package (internal/cli, one file per command); each service gets a typed (*app.Services).Mount<Svc> method and its own internal/cli/svc_<name>.go subcommand that passes that method EXPRESSION to a shared serve() helper. serverkit OWNS OTel. Use when bumping across the typed-composed-server release.
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

**After.** serverkit runs a *composed* server, owns no selection, and OWNS
OTel — it takes an already-built handler plus the selected workers/operators
and wires OpenTelemetry itself from `Config`:

```go
// forge/pkg/serverkit
type Server struct {
    Handler      http.Handler                  // mux with everything already mounted
    Logger       *slog.Logger
    Workers      []Worker                       // already-selected
    Operators    []Operator                     // already-selected
    RunOperators func(ctx context.Context, logger *slog.Logger, healthProbeAddr string) error
    OnShutdown   func(context.Context) error
    // + project edge-middleware factories (CORS / SecurityHeaders / RequestID).
}
type Config struct {
    // ... lifecycle knobs ...
    OTLPEndpoint   string // serverkit calls observe.Setup internally
    ServiceName    string // app identity (semconv service.name)
    ServiceVersion string
}
func Run(ctx context.Context, cfg Config, srv Server) error   // no hooks, no args, no names
```

Gone entirely:

- `serverkit.Application`, the `Hooks` struct, the `args []string` / `names`
  parameter on `Run`, and the old `appkit` `Options.Only` string filter.
- The **string→inventory mount lookup** on the run path. The old cmd
  iterated `app.Inventory` matching `row.Name` against `args` and called
  `row.Mount(...)`. That is replaced by TYPED mounts (below).
- The **generated `cmd/otel.go` shim**. serverkit now calls `observe.Setup`
  internally from `Config.OTLPEndpoint` + `Config.ServiceName`, mounts
  `/metrics` on its own edge, and flushes the providers at shutdown. The cmd
  just projects `cfg.OtlpEndpoint` + the generated `ServiceName` constant.
- The whole `cmd/*.go` layout. The command tree moves to a real cobra
  **`internal/cli` package** (gh / cobra-cli idiom: one file per command);
  `cmd/<bin>/main.go` becomes a thin `cli.Execute()`.

**TYPED selection.** `internal/app/inventory_gen.go` now emits a typed method
per service plus an explicit `MountAll`:

```go
func (s *Services) MountBilling(mux, cfg, logger, opts...) []string { ... return path }
func (s *Services) MountAll(mux, cfg, logger, opts...) []string { /* explicit typed calls */ }
```

`app.Inventory` STAYS but is **data-only** (no `Mount` closure) — introspection
for `forge map` / `audit` / `services` listing only. It is NEVER on the run
path.

The generated cmd layout:

- `internal/cli/root.go` — `newRootCmd(deps)` assembles the tree; defines the
  `ServiceName` constant (app identity) + a `Deps` struct (config/io, threaded
  for testability).
- `internal/cli/serve.go` — the shared `serve(ctx, deps, mount mountFunc, ...)`
  helper: OpenInfra → Build → PostBuild → interceptor chain → apply the TYPED
  mount FUNCTION → `serverkit.Run`. Takes a typed mount **function value**,
  never a string.
- `internal/cli/server.go` — the all-services command → `serve(ctx, deps,
  (*app.Services).MountAll, ...)`. The optional `server [names...]` subset uses
  a generated `app.MountByName` map of **typed method expressions** (not a
  string→data lookup).
- `internal/cli/svc_<name>.go` — **ONE FILE PER SERVICE**: `new<Svc>Cmd(deps)`
  whose `RunE` calls `serve(ctx, deps, (*app.Services).Mount<Svc>)` — a method
  EXPRESSION, fully typed, no string. `<bin> <service>` is a first-class
  command with its own `-h`. A service whose kebab name collides with a
  built-in (`server`/`version`/`db`/`help`/`completion`) is skipped with a NOTE
  in `svc_register_gen.go` and remains reachable via `server <name>`.

```go
// internal/cli/svc_billing.go (GENERATED) — typed per-service subcommand.
func newBillingCmd(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use: "billing",
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.Cmd = cmd
			return serve(cmd.Context(), deps, (*app.Services).MountBilling, serveOptions{})
		},
	}
}
```

The forked `cmd/workspace-proxy/main.go` outlier folds away too: with a
handler-shaped serverkit it builds its own handler and passes a
`serverkit.Server` like everyone else (~200 lines of duplicated lifecycle
deleted).

## 2. Detection

```bash
# Old shape — name/hooks-driven Run + string→inventory mount + otel shim.
grep -rq "serverkit.Run(ctx, cfg, hooks" cmd/ && echo "OLD SHAPE — hooks+args Run"
grep -rq "BootstrapOnly" pkg/app/ cmd/ && echo "OLD SHAPE — string selection"
grep -rq "runServer(cmd, \[\]string{" cmd/ && echo "OLD SHAPE — string-projection subcommands"
grep -rq "for _, row := range app.Inventory" cmd/ && echo "OLD SHAPE — string→inventory mount lookup"
test -f cmd/otel.go && echo "OLD SHAPE — generated cmd/otel.go shim"

# New shape — typed mounts + internal/cli package + serverkit-owned OTel.
test -d internal/cli && echo "internal/cli command package present"
grep -rq "(\*app.Services).Mount" internal/cli/ && echo "typed mount method expressions in use"
grep -rq "func (s \*Services) MountAll" internal/app/inventory_gen.go && echo "typed MountAll present"
```

## 3. Migration (deterministic part)

```bash
# 1. Bump forge_version in forge.yaml to the typed-composed-server release.
# 2. Regenerate. The generator: emits the internal/cli command tree (root.go,
#    serve.go, server.go, version.go, db.go, one svc_<name>.go per service);
#    emits typed (*app.Services).Mount<Svc> + MountAll in inventory_gen.go;
#    rewrites app.Inventory as data-only; thins cmd/<bin>/main.go to a
#    cli.Execute(); and DELETES cmd/server.go + cmd/otel.go + cmd/services_gen.go.
forge generate

# 3. Build.
go build ./...
```

For stock projects (un-forked `cmd/`) the regen is the whole migration — the
new `internal/cli` package composes `serverkit.Server` for you and serverkit
owns OTel.

## 4. Migration (manual part — forked cmd/server.go)

The painful case is a **disowned `cmd/server.go`** (control-plane is the
canonical example — a forked composition root plus the hand-rolled
`cmd/workspace-proxy/main.go`). `forge generate` leaves disowned files alone;
apply the shape change by hand, mirroring the generated `internal/cli/serve.go`:

1. **Build the mux yourself, then pass it as `Server.Handler`.** Whatever
   `Hooks.Bootstrap` used to do — build the mux, mount services, attach
   interceptors — now happens in the cmd layer and the result is a plain
   `http.Handler`. Apply a TYPED mount (a method value), never a string:

   ```go
   // before
   serverkit.Run(ctx, cfg, hooks, args)

   // after — build mux + interceptors, run DI, apply the typed mount func.
   mux := http.NewServeMux()
   // ... interceptor chain (serverkit owns /metrics + OTel now) ...
   infra, _   := app.OpenInfra(ctx, cfg, logger)
   services, _ := app.Build(infra)
   _ = app.PostBuild(services)
   mounted := mount(services, mux, cfg, logger, opts...) // mount = (*app.Services).MountAll
   serverkit.Run(ctx, skCfg, serverkit.Server{
       Handler:   mux,        // or a REST transcoder wrapping it
       Logger:    logger,
       Workers:   app.WorkerList(services),
       Operators: app.OperatorList(services),
   })
   ```

2. **Replace string selection with typed mounts.** Anywhere you relied on
   `BootstrapOnly(names)` / `Options.Only` / a `for row := range app.Inventory`
   name-match loop, call the typed `(*app.Services).Mount<Svc>` method for the
   single-service case or `MountAll` for all. Construction already happened in
   `app.Build`; the typed mount only decides which routes get registered.
   Delete the name-matching mount path. For OTel, drop your `setupOTel` call —
   project `cfg.OtlpEndpoint` + a `ServiceName` constant onto
   `serverkit.Config` and let serverkit own it.

3. **Map old hooks onto the composed inputs:**

   | Old hook / mechanism                | New home                                              |
   | ----------------------------------- | ----------------------------------------------------- |
   | `Hooks.Bootstrap` / `BootstrapOnly` | cmd builds `Server.Handler`; applies a TYPED mount func |
   | `Hooks.PostBootstrap`               | call it inline after building the handler             |
   | string→inventory mount loop         | `(*app.Services).Mount<Svc>` / `MountAll` (typed)    |
   | worker / operator selection         | `app.WorkerList` / `app.OperatorList` → `Server.Workers` / `.Operators` |
   | operator manager entry point        | `Server.RunOperators` (→ `app.RunOperators(services, …)`) |
   | graceful-shutdown hook              | `Server.OnShutdown`                                   |
   | `setupOTel` / `cmd/otel.go`         | **serverkit owns OTel** — set `Config.OTLPEndpoint` + `Config.ServiceName` |
   | `AutoMigrate`                       | cmd owns migrate; `Config` knobs unchanged           |

4. **Preserve interceptor ordering explicitly.** The old path prepended the
   canonical `observe.DefaultMiddlewares` chain and ran the project
   interceptors after it (otelconnect → rate-limit → auth → audit). With the
   handler built in cmd, *you* now own that ordering when constructing the
   interceptor chain and threading it as `HandlerOption`s into the mount — keep
   it identical. This is the easiest thing to silently regress.

5. **Fold `cmd/workspace-proxy/main.go` into a subcommand.** Replace the
   forked `main()` (OTel setup, signal handling, healthz/readyz, metrics
   server, shutdown — all of which serverkit owns) with a cobra subcommand in
   `internal/cli` that builds the proxy's own mux/deps and hands serverkit a
   `serverkit.Server`. The only thing that stays is the proxy's own
   handler-building composition root; everything else deletes.

## 5. Verification

```bash
go build ./... && go test ./... && forge lint
```

Shape checks:

```bash
grep -rq "serverkit.Server{" internal/cli/ && echo "composed Server in use"
! test -f cmd/services_gen.go && ! test -f cmd/server.go && echo "old cmd/ server files gone"
! test -f cmd/otel.go && echo "otel shim gone (serverkit owns OTel)"
! grep -rq "for _, row := range app.Inventory" internal/cli/ && echo "no string→inventory mount lookup on run path"
grep -rq "(\*app.Services).Mount" internal/cli/ && echo "typed mount method expressions in use"
ls internal/cli/svc_*.go && echo "one file per service"

# Typed per-service subcommands + the all-services command.
go run ./cmd/<bin> billing        # boots ONLY billing via the typed mount
go run ./cmd/<bin> server         # mounts everything
```

Smoke the lifecycle: start the `server` command, hit `/healthz`, `/readyz`,
and `/metrics` (serverkit-owned), send SIGTERM, confirm graceful drain.

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
