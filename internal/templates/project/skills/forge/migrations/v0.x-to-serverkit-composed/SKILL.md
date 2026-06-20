---
name: v0.x-to-serverkit-composed
description: Migrate serverkit from Run(ctx, cfg, hooks, args) — where args were service NAMES matched through a string registry — to Run(ctx, cfg, serverkit.Server{Handler, Workers, Operators, OnShutdown}). The Application interface, Hooks, and string-keyed BootstrapOnly selection are gone; service/worker/operator SELECTION moves UP into the cmd layer. Each cobra subcommand becomes a real, owned composition root. Use when bumping across the composed-server release.
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
cobra wrappers (`RunE: runServer(cmd, []string{"billing"})`) — every
subcommand showed generic `server -h` help, no real identity. Selection was
welded into the framework entrypoint *and* the DI graph.

**After.** serverkit runs a *composed* server and owns no selection:

```go
// forge/pkg/serverkit
type Server struct {
    Handler    http.Handler                 // mux with everything already mounted
    Workers    []Worker
    Operators  []Operator
    OnShutdown func(context.Context) error
    // (plus the same lifecycle config knobs as before)
}
func Run(ctx context.Context, cfg Config, srv Server) error   // no hooks, no args, no names
```

Gone entirely:

- `serverkit.Application` interface and the `Hooks` struct (Bootstrap /
  BootstrapOnly dispatch by name).
- The `args []string` / `names` parameter.
- `cmd/services_gen.go` string-projection wrappers and the `appkit`
  `Options.Only` string filter.

Selection moves **up into the cmd layer**: each cobra subcommand is now a real,
owned composition root that mounts exactly what it serves and hands serverkit a
ready `Server`. A tiny `serverCmd(name, mountFn)` helper keeps each subcommand
~5 lines but real (own flags, own help, a Go symbol you can jump to):

```go
// cmd/billing.go — owned, idiomatic
func newBillingCmd() *cobra.Command { return serverCmd("billing", app.MountBilling) }

// cmd/server.go — the multi-mount "all services" command composes every MountX
```

The forked `cmd/workspace-proxy/main.go` outlier folds away too: with a
handler-shaped serverkit it becomes a peer subcommand that builds its own
handler and passes a `serverkit.Server` like everyone else (~200 lines of
duplicated lifecycle deleted).

## 2. Detection

```bash
# Old shape — name/hooks-driven Run.
grep -q "serverkit.Run(ctx, cfg, hooks" cmd/server.go && echo "OLD SHAPE — hooks+args Run"
grep -q "BootstrapOnly" pkg/app/*.go && echo "OLD SHAPE — string selection"
test -f cmd/services_gen.go && echo "OLD SHAPE — string-projection subcommands"

# New shape — composed Server.
grep -rq "serverkit.Server{" cmd/ && echo "already on composed Server"
```

## 3. Migration (deterministic part)

```bash
# 1. Bump forge_version in forge.yaml to the composed-server release.
# 2. Regenerate. forge emits real per-service cmd/<svc>.go (or the serverCmd
#    helper + MountX hooks), the composed cmd/server.go, and deletes
#    cmd/services_gen.go.
forge generate

# 3. Build.
go build ./...
```

For stock projects (un-forked `cmd/` and `pkg/app/bootstrap.go`) the regen is
the whole migration — the new files compose `serverkit.Server` for you.

## 4. Migration (manual part — forked cmd/server.go or bootstrap)

The painful case is a **disowned `cmd/server.go` or `pkg/app/bootstrap.go`**
(control-plane is the canonical example — forked `bootstrap.go` with
`constructWorkers` / `mountDaemonRegistryAdapter`, plus the hand-rolled
`cmd/workspace-proxy/main.go`). `forge generate` leaves disowned files alone;
apply the shape change by hand:

1. **Build the mux yourself, then pass it as `Server.Handler`.** Replace the
   old hooks dispatch with explicit mounting. Whatever `Hooks.Bootstrap` used
   to do — build the mux, mount services, attach interceptors — now happens in
   the cmd-layer composition root and the result is a plain `http.Handler`:

   ```go
   // before
   serverkit.Run(ctx, cfg, hooks, args)

   // after
   handler, workers, operators := buildServer(ctx, cfg)   // your composition root
   serverkit.Run(ctx, cfg, serverkit.Server{
       Handler:    handler,
       Workers:    workers,
       Operators:  operators,
       OnShutdown: shutdownFn,
   })
   ```

2. **Move selection out of the DI graph into cmd.** Anywhere you relied on
   `BootstrapOnly(names)` / `Options.Only` to mount a subset, replace it with a
   subcommand that calls only the `MountX` closures it wants. The `server`
   command mounts all of them; `billing` mounts `app.MountBilling`; etc. Delete
   the name-matching path.

3. **Map old hooks onto the composed inputs:**

   | Old hook / mechanism                | New home                                            |
   | ----------------------------------- | --------------------------------------------------- |
   | `Hooks.Bootstrap` / `BootstrapOnly` | cmd-layer composition root building `Server.Handler`|
   | `Hooks.PostBootstrap`               | call it inline after building the handler           |
   | worker / operator selection         | populate `Server.Workers` / `Server.Operators`      |
   | graceful-shutdown hook              | `Server.OnShutdown`                                  |
   | `SetupOTel`, `AutoMigrate`          | unchanged serverkit `Config` knobs / cmd setup      |

4. **Preserve interceptor ordering explicitly.** The old path prepended the
   canonical `observe.DefaultMiddlewares` chain and ran `ProjectInterceptors`
   after it (otelconnect → rate-limit → auth → audit). With the handler built
   in cmd, *you* now own that ordering when constructing the mux/handler — keep
   it identical. This is the easiest thing to silently regress.

5. **Fold `cmd/workspace-proxy/main.go` into a subcommand.** Replace the
   forked `main()` (OTel setup, signal handling, healthz/readyz, metrics
   server, shutdown — all of which serverkit owns) with:

   ```go
   func newWorkspaceProxyCmd() *cobra.Command {
       return serverCmd("workspace-proxy", buildProxyHandler)  // its own mux + deps
   }
   ```

   The only thing that stays is `buildProxyHandler` (the proxy's own
   composition root). Everything else deletes.

## 5. Verification

```bash
go build ./... && go test ./... && forge lint
```

Shape checks:

```bash
grep -rq "serverkit.Server{" cmd/ && echo "composed Server in use"
! test -f cmd/services_gen.go && echo "string-projection subcommands gone"
! grep -rq "BootstrapOnly\|Options{Only" pkg/app/ && echo "string selection gone"

# Each subcommand is real (own help), not a generic server projection.
go run . billing -h        # should show billing-specific help, not generic server -h
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
  cmd (this skill) and wiring moving to an owned typed composition root (that
  skill) are two halves of the same redesign; do layout, then this, then DI.
- `binaries` skill — `forge add binary` / `forge add worker` and the
  per-subcommand composition-root pattern.
