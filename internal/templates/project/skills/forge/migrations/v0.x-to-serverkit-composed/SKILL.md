---
name: v0.x-to-serverkit-composed
description: Migrate serverkit from Run(ctx, cfg, hooks, args) — where args were service NAMES matched through a string registry — to Run(ctx, cfg, serverkit.Server{...}) with FULLY TYPED service selection. The Application interface, Hooks, string-keyed BootstrapOnly selection, the string→inventory mount lookup, AND the generated cmd/otel.go shim are all gone. The cmd layer becomes a real cobra command tree under cmd/<bin>/cmd (devspace idiom), dir-nested by category; each service gets a typed (*app.Components).Mount<Svc> method and its own cmd/<bin>/cmd/services/<name>.go subcommand that passes that method EXPRESSION to a shared cmd.Serve() helper. serverkit OWNS OTel. Use when bumping across the typed-composed-server release.
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
- The whole flat `cmd/*.go` layout. The command tree moves under
  **`cmd/<bin>/cmd`** as a real cobra package, dir-nested by category
  (devspace idiom): `root.go`/`serve.go`/`server.go`/`version.go`/`db.go`/
  `commands.go` in `package cmd`, plus `services/`, `workers/`, `operators/`
  SUBPACKAGES with one file per item. `cmd/<bin>/main.go` becomes a thin
  `cmd.Execute()` that blank-imports the group subpackages so each item
  self-registers via `init()`. (An earlier vintage put this tree in a flat
  `internal/cli` package — if you're on that, the regen moves it for you.)

**TYPED selection.** `internal/app/mounts_services.go` now emits a typed method
per service plus an explicit `MountAll`:

```go
func (c *Components) MountBilling(mux, cfg, logger, opts...) []string { ... return path }
func (c *Components) MountAll(mux, cfg, logger, opts...) []string { /* explicit typed calls */ }
```

`app.Inventory` STAYS but is **data-only** (no `Mount` closure) — introspection
for `forge map` / `audit` / `services` listing only. It is NEVER on the run
path.

The generated cmd layout (devspace idiom — dir-nested by category):

```
cmd/<bin>/
  main.go            # thin: package main → cmd.Execute() + blank group imports
  cmd/               # package cmd (the command tree)
    root.go          # newRootCmd(deps); ServiceName const; Deps struct; the
                     # Register{Service,Worker,Operator}Cmd registry
    serve.go         # the shared Serve(ctx, deps, mount MountFunc, ...) helper
    server.go        # all-services → Serve(deps, (*app.Components).MountAll)
    version.go, db.go, commands.go
    services/<svc>.go    # ONE FILE PER SERVICE (package services)
    workers/<w>.go       # one file per worker (package workers)
    operators/<o>.go     # one file per operator (package operators)
```

- `cmd/<bin>/cmd/root.go` — `newRootCmd(deps)` ranges the registry the group
  subpackages populated at `init()`; defines the `ServiceName` constant (app
  identity) + a `Deps` struct (config/io, threaded for testability) + the
  `Register{Service,Worker,Operator}Cmd` registry.
- `cmd/<bin>/cmd/serve.go` — the shared, EXPORTED `Serve(ctx, deps, mount
  MountFunc, ...)` helper: OpenInfra → NewComponents → interceptor chain →
  apply the TYPED mount FUNCTION → `serverkit.Run`. Takes a typed mount
  **function value**, never a string. Also exports `ServeOptions`,
  `SelectWorkers`, `SelectOperators`, and `MountNone`.
- `cmd/<bin>/cmd/server.go` — the all-services command → `Serve(ctx, deps,
  (*app.Components).MountAll, ...)`. The optional `server [names...]` subset uses
  a generated `app.MountByName` map of **typed method expressions** (not a
  string→data lookup).
- `cmd/<bin>/cmd/services/<name>.go` — **ONE FILE PER SERVICE** (package
  `services`): `New<Svc>Cmd(cmd.Deps)` whose `RunE` calls `cmd.Serve(ctx, deps,
  (*app.Components).Mount<Svc>)` — a method EXPRESSION, fully typed, no string.
  It self-registers via `init()` (`cmd.RegisterServiceCmd`). `<bin> <service>`
  is a first-class command with its own `-h`. A service whose kebab name
  collides with a built-in (`server`/`version`/`db`/`help`/`completion`) is
  skipped with a NOTE in `services/register_gen.go` and remains reachable via
  `server <name>`. Workers/operators get parallel `workers/<w>.go` /
  `operators/<o>.go` files (cmd.MountNone + a named supervised subset).

```go
// cmd/<bin>/cmd/services/billing.go (GENERATED) — typed per-service subcommand.
package services

func init() { cmd.RegisterServiceCmd(NewBillingCmd) }

func NewBillingCmd(deps cmd.Deps) *cobra.Command {
	c := &cobra.Command{
		Use: "billing",
		RunE: func(c *cobra.Command, args []string) error {
			deps.Cmd = c
			return cmd.Serve(c.Context(), deps, (*app.Components).MountBilling, cmd.ServeOptions{})
		},
	}
	config.RegisterFlags(c)
	return c
}
```

Each binary gets its OWN `cmd/<bin>/` tree: the forked
`cmd/workspace-proxy/main.go` outlier folds away — `forge add binary` now
scaffolds a self-contained `cmd/<bin>/main.go`, and the primary server binary's
per-service commands live under the server's tree.

## 2. Detection

```bash
# Old shape — name/hooks-driven Run + string→inventory mount + otel shim.
grep -rq "serverkit.Run(ctx, cfg, hooks" cmd/ && echo "OLD SHAPE — hooks+args Run"
grep -rq "BootstrapOnly" pkg/app/ cmd/ && echo "OLD SHAPE — string selection"
grep -rq "runServer(cmd, \[\]string{" cmd/ && echo "OLD SHAPE — string-projection subcommands"
grep -rq "for _, row := range app.Inventory" cmd/ && echo "OLD SHAPE — string→inventory mount lookup"
test -f cmd/otel.go && echo "OLD SHAPE — generated cmd/otel.go shim"

# New shape — typed mounts + cmd/<bin>/cmd command tree + serverkit-owned OTel.
ls -d cmd/*/cmd && echo "cmd/<bin>/cmd command tree present"
grep -rq "(\*app.Components).Mount" cmd/*/cmd/ && echo "typed mount method expressions in use"
grep -rq "func (c \*Components) MountAll" internal/app/mounts_services.go && echo "typed MountAll present"
ls cmd/*/cmd/services/*.go && echo "one file per service (services/ group)"
```

## 3. Migration (deterministic part)

```bash
# 1. Bump forge_version in forge.yaml to the typed-composed-server release.
# 2. Regenerate. The generator: emits the cmd/<bin>/cmd command tree (root.go,
#    serve.go, server.go, version.go, db.go, commands.go) + the services/,
#    workers/, operators/ group subpackages (one file per item); emits typed
#    (*app.Components).Mount<Svc> + MountAll in mounts_services.go; rewrites
#    app.Inventory as data-only; thins cmd/<bin>/main.go to a cmd.Execute()
#    that blank-imports the groups; and DELETES the old flat internal/cli
#    tree + cmd/server.go + cmd/otel.go + cmd/services_gen.go.
forge generate

# 3. Build.
go build ./...
```

For stock projects (un-forked `cmd/`) the regen is the whole migration — the
new `cmd/<bin>/cmd` command tree composes `serverkit.Server` for you and
serverkit owns OTel.

## 4. Migration (manual part — forked cmd/server.go)

The painful case is a **disowned `cmd/server.go`** (control-plane is the
canonical example — a forked composition root plus the hand-rolled
`cmd/workspace-proxy/main.go`). `forge generate` leaves disowned files alone;
apply the shape change by hand, mirroring the generated `cmd/<bin>/cmd/serve.go`:

1. **Build the mux yourself, then pass it as `Server.Handler`.** Whatever
   `Hooks.Bootstrap` used to do — build the mux, mount services, attach
   interceptors — now happens in the cmd layer and the result is a plain
   `http.Handler`. Apply a TYPED mount (a method value), never a string:

   ```go
   // before
   serverkit.Run(ctx, cfg, hooks, args)

   // after — build mux + interceptors, run DI, apply the typed mount func.
   mux := http.NewServeMux()
   // ... interceptor chain via observe.Chain (serverkit owns /metrics + OTel now) ...
   infra, _      := app.OpenInfra(ctx, cfg, logger)
   components, _ := app.NewComponents(infra)
   // two-phase setters (if any): forge disown internal/app/compose.go and
   // wire them inline in NewComponents — there is no PostBuild hook.
   mounted := mount(components, mux, cfg, logger, opts...) // mount = (*app.Components).MountAll
   serverkit.Run(ctx, skCfg, serverkit.Server{
       Handler:   mux,        // or a REST transcoder wrapping it
       Logger:    logger,
       Workers:   app.WorkerList(components),
       Operators: app.OperatorList(components),
   })
   ```

2. **Replace string selection with typed mounts.** Anywhere you relied on
   `BootstrapOnly(names)` / `Options.Only` / a `for row := range app.Inventory`
   name-match loop, call the typed `(*app.Components).Mount<Svc>` method for the
   single-service case or `MountAll` for all. Construction already happened in
   `app.NewComponents`; the typed mount only decides which routes get registered.
   Delete the name-matching mount path. For OTel, drop your `setupOTel` call —
   project `cfg.OtlpEndpoint` + a `ServiceName` constant onto
   `serverkit.Config` and let serverkit own it.

3. **Map old hooks onto the composed inputs:**

   | Old hook / mechanism                | New home                                              |
   | ----------------------------------- | ----------------------------------------------------- |
   | `Hooks.Bootstrap` / `BootstrapOnly` | cmd builds `Server.Handler`; applies a TYPED mount func |
   | `Hooks.PostBootstrap`               | call it inline after building the handler             |
   | string→inventory mount loop         | `(*app.Components).Mount<Svc>` / `MountAll` (typed)    |
   | worker / operator selection         | `app.WorkerList` / `app.OperatorList` → `Server.Workers` / `.Operators` |
   | operator manager entry point        | `Server.RunOperators` (→ `app.RunOperators(components, …)`) |
   | graceful-shutdown hook              | `Server.OnShutdown`                                   |
   | `setupOTel` / `cmd/otel.go`         | **serverkit owns OTel** — set `Config.OTLPEndpoint` + `Config.ServiceName` |
   | `AutoMigrate`                       | cmd owns migrate; `Config` knobs unchanged           |

4. **Preserve interceptor ordering explicitly.** Build the chain with
   `observe.Chain(observe.Deps{Logger, Auth, Audit, RateLimit, Extras})` — the
   application interceptors handed in BY NAMED FIELD (no `Set*` globals). The
   canonical forge order is recovery → request-id → logging → tracing → metrics,
   then auth → audit → rate-limit (otelconnect rides `Extras`). With the
   handler built in cmd, *you* own that chain and thread it as `HandlerOption`s
   into the mount — keep
   it identical. This is the easiest thing to silently regress.

5. **Thin `cmd/workspace-proxy/main.go`.** Each binary gets its own
   `cmd/<bin>/` tree. Replace the forked `main()` (OTel setup, signal handling,
   healthz/readyz, metrics server, shutdown — all of which serverkit owns) with
   a thin main that builds the proxy's own mux/deps and hands serverkit a
   `serverkit.Server`. The only thing that stays is the proxy's own
   handler-building composition root; everything else deletes. (`forge add
   binary` scaffolds exactly this self-contained shape.)

## 5. Verification

```bash
go build ./... && go test ./... && forge lint
```

Shape checks:

```bash
grep -rq "serverkit.Server{" cmd/*/cmd/ && echo "composed Server in use"
! test -f cmd/services_gen.go && ! test -f cmd/server.go && echo "old flat cmd/ server files gone"
! test -d internal/cli && echo "old flat internal/cli tree gone"
! test -f cmd/otel.go && echo "otel shim gone (serverkit owns OTel)"
! grep -rq "for _, row := range app.Inventory" cmd/*/cmd/ && echo "no string→inventory mount lookup on run path"
grep -rq "(\*app.Components).Mount" cmd/*/cmd/ && echo "typed mount method expressions in use"
ls cmd/*/cmd/services/*.go && echo "one file per service"

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
