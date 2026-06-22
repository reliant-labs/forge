---
name: v0.x-to-typed-di
description: Migrate DI from wire_gen.go name-matching + the appkit string-keyed DI table to a GENERATED type-topological composition (internal/app/compose.go, func NewComponents(infra) (*Components, error)) that resolves each Deps field BY TYPE from constructed components or fields on an OWNED Infra struct (internal/app/providers.go, OpenInfra). The old pkg/app DI unit is fully removed; a missing required provider is now a loud error (generate-time when provable, compile-time otherwise), not a silent nil. Move setup.go infra construction into Infra/OpenInfra; two-phase setters (e.g. billing.WithReliantAPIKeyIssuer(llm)) are wired by `forge disown internal/app/compose.go` and editing the construction site. Use when bumping across the typed-DI release.
relevance: migration
---

# Migrating to the typed, type-topological DI injector

Use this skill when `forge upgrade` reports a jump across the release that
replaces name-matched `wire_gen.go` + the `appkit` string-keyed DI table with a
generated **type-topological** injector plus an **owned** provider set. This
is the DI half of the project-shape redesign; the cmd/selection half is
`migrations/v0.x-to-serverkit-composed`. Do layout first, then serverkit-composed,
then this.

## 1. What changed

**Before.** Two lossy magic layers stacked on top of the real wiring:

- `pkg/app/wire_gen.go` matched each service's `Deps` fields to `*App` fields
  **by name and type**. A consumer declaring a narrow `Repository` interface
  while `*App` held the concrete `*db.PostgresRepository` was **silently
  skipped** — a typed-nil hazard live in production.
- `appkit` held a string-keyed DI table (`appkit.Def` / `ServiceDef` /
  `appkit.Run`); selection by name string was welded into construction.

The actual wiring already lived in a hand-owned `setup.go` (often hundreds of
lines); `wire_gen`'s name-match was a lossy layer on top of it.

**After.** The OLD `pkg/app` DI unit is **fully removed** — `bootstrap.go`,
`wire_gen.go`, `services_gen.go`, `services.go`, and the
`appkit.Def`/`ServiceDef`/`appkit.Run` table are all deleted (the `appkit`
package still exists, but only for worker-wrapping via `appkit.WrapWorker`; its
DI table is gone). The live DI now lives entirely under `internal/app`:

- **A generated type-topological composition** — `internal/app/compose.go`,
  `func NewComponents(infra *Infra) (*Components, error)`. It constructs every
  registered component in type-topological order and resolves each `Deps` field
  **by type, structurally** — from a constructed producer, from a field on the
  owned `*Infra` struct, or from the conventional sources (`Logger` →
  `infra.Log`, `Config` → `infra.Cfg`). There is no string lookup and no
  name-match. `Components` is a plain typed bag (one field per component), NOT a
  god-struct. This file is **forge-owned and regenerated every run** —
  adding/removing a component is a `forge generate`, never a hand-edit (unless
  you `forge disown` it, below).
- **An owned `Infra` struct + `OpenInfra`** in `internal/app/providers.go`.
  `Infra` is a data struct of everything the composition cannot derive — DB pool,
  NATS conn, k8s/third-party clients, adapter-wrapped repos, explicit
  concrete→interface bindings. `OpenInfra(ctx, cfg, logger)` constructs them.
  forge scaffolds this file ONCE and never overwrites it — it's yours.
  Resolution is **by type from the Infra FIELDS**, not per-type `provideXxx`
  functions: a concrete `*db.PostgresRepository` field on `Infra` fills every
  narrow `Repository` interface a consumer declares, proven assignable.
- **Two-phase setters / cycle back-edges by disowning `compose.go`.** The graph
  is a DAG, but a few wirings are post-construction setters / cycle back-edges —
  e.g. `billing.WithReliantAPIKeyIssuer(llm)`, `authbridge ← billing`. Pure
  constructor topo-ordering can't place those. There is NO separate `PostBuild`
  hook: `forge disown internal/app/compose.go` to own the bytes, then add the
  setter inline after both endpoints are constructed. (For a back-edge the
  generator detected, the emitted `compose.go` already leaves a `HasCycle`
  comment naming the edge to wire.)

```go
// internal/app/providers.go (OWNED — scaffolded once, you maintain it)
type Infra struct {
    Log *slog.Logger
    Cfg *config.Config
    DB  *sql.DB
    // add a field for every collaborator NewComponents reports as "no provider":
    // a repository, a NATS publisher, a third-party client, an adapter wrapping.
    Repo *db.PostgresRepository   // fills any narrow Repository Deps field BY TYPE
}
func OpenInfra(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*Infra, error) {
    infra := &Infra{Log: logger, Cfg: cfg}
    // open DB/NATS/clients, wrap adapters, pick explicit bindings, assign onto infra.
    return infra, nil
}

// internal/app/compose.go (GENERATED — type-topological; disown to hand-own)
func NewComponents(infra *Infra) (*Components, error) {
    c := &Components{}
    c.Audit = audit.New(audit.Deps{Repo: infra.Repo})  // infra.Repo satisfies audit.Repository by type
    // … topo order resolved by type, each component constructed once …
    // After `forge disown`, a two-phase setter lives right here:
    //   c.Billing.WithReliantAPIKeyIssuer(c.LLM)   // phase 2, explicit + visible
    return c, nil
}
```

`cmd/<bin>/cmd/serve.go` calls `app.OpenInfra → app.NewComponents` in sequence,
then applies a typed `(*app.Components).Mount<Svc>` mount method.

## 2. Detection

```bash
# Old shape — name-matched wire_gen + appkit DI table.
test -f pkg/app/wire_gen.go && echo "OLD SHAPE — name-matched wire_gen"
grep -rq "appkit.Def\|ServiceDef\|appkit.Run\|Options{Only\|BootstrapOnly" pkg/app/ cmd/ && echo "OLD SHAPE — appkit DI table"

# New shape — typed composition + owned Infra/OpenInfra.
test -f internal/app/compose.go && echo "generated composition present"
grep -rq "func NewComponents" internal/app/compose.go && echo "NewComponents present"
grep -rq "func OpenInfra\|type Infra struct" internal/app/providers.go && echo "owned Infra present"
```

## 3. Migration (deterministic part)

```bash
# 1. Bump forge_version in forge.yaml to the typed-DI release.
# 2. Regenerate. forge DELETES the old pkg/app DI unit (bootstrap.go,
#    wire_gen.go, services_gen.go, services.go + the appkit DI table), emits
#    the explicit composition under internal/app (compose.go = NewComponents,
#    mounts_services.go = Mount<Svc> + the data-only Inventory, lifecycle.go),
#    and SCAFFOLDS internal/app/providers.go (Infra + OpenInfra) ONCE if it
#    doesn't exist. The now-orphaned pkg/app/setup.go is left compiling until
#    you port its construction into OpenInfra.
forge generate

# 3. Build — expect errors here on first pass; they are the migration's TODO
#    list (every missing required provider is a loud error). See section 4.
go build ./...
```

The regen is intentionally *not* a clean build for non-trivial projects: the
whole point is that previously-silent name-match gaps now surface as errors you
must resolve by adding a field to `Infra`. A required Deps field that resolves
to no producer, no `Infra` field, and no conventional source is **loud** —
generate-time when the matcher can PROVE `Infra` has no assignable field
(generation errors, naming the type + component + field), otherwise a
compile-time backstop (`NewComponents` emits `infra.<Field>` and the Go compiler
arbitrates). It never emits a silent typed-zero for a required field. (Scalar
Deps fields are configuration, not collaborators — they take the typed-zero and
never raise a missing-provider error.)

## 4. Migration (manual part)

This is the real work. control-plane/kalshi run this after the forge/pkg bump.

1. **Move setup.go infra construction into `Infra` + `OpenInfra`.** Your old
   `pkg/app/setup.go` built infrastructure into locals/`*App` fields (DB pool,
   NATS, audit sink, adapter-wrapped repos, dialers nil'd on unset env). The
   slim `app_gen.go` carrier keeps setup.go *compiling*, but it is now
   **orphaned** — nothing in the live boot path calls it. Move each
   construction into a field on the `Infra` struct, built in `OpenInfra`, in
   `internal/app/providers.go`. The construction logic itself is unchanged —
   only its home moves. Adapter wrapping (`enforcement.NewDBRepository(repo)`)
   and env-conditional dialers go in `OpenInfra` verbatim, assigned onto
   `infra` fields. Once setup.go's content has moved, the orphaned file can be
   deleted.

2. **Move two-phase setters into a disowned `compose.go`.** Any
   post-construction injection — `billing.WithReliantAPIKeyIssuer(llm)`,
   `authbridge ← billing`, any `X.SetY(z)` that ran in setup.go *after* both X
   and Y existed — has no separate hook. `forge disown internal/app/compose.go`
   and add the setter inline in `NewComponents` after both components are
   constructed onto `c`. (forge flags an unwired cycle back-edge with a
   `HasCycle` comment in the emitted file so you know what to wire.)

3. **Fix the narrow-interface fills that name-match used to drop.** Where a
   consumer declares a narrow `Repository` interface and `Infra` holds the
   concrete `*db.PostgresRepository`, the type system now checks it: either the
   concrete type is assignable to the narrow interface (resolves) or it isn't
   (loud — fix the interface or the Infra field). This is the silent-nil bug
   becoming an error; resolve each one rather than working around it.

4. **Per-binary singletons.** A collaborator that must be one instance *within
   each* of two binaries (e.g. `enforcement` — one in the server, a separate
   one in workspace-proxy) is built once per binary's composition: each
   binary calls `OpenInfra` + `NewComponents` and gets its own `*Components`.
   "One instance per graph" is the natural outcome — don't try to share across
   processes.

5. **Cross-binary / split-out collaborators.** A binary that should NOT build a
   service in-process fills that collaborator's interface with a Connect client
   instead of the real service — assign the client onto an `Infra` field of the
   interface type (`user.Service` ← `userclient.New(conn)`). In-process is the
   within-binary default; the interface seam makes the boundary cheap.

6. **Confirm the old artifacts are gone.** Once it builds: confirm
   `pkg/app/wire_gen.go`, `services_gen.go`, `services.go`, `bootstrap.go`, the
   `appkit` DI table (`appkit.Def`/`ServiceDef`/`appkit.Run`), and any
   `Options.Only` / `BootstrapOnly` call sites are gone — `forge generate`
   deleted them. (The serverkit-composed migration removed the selection call
   sites; this one removed the construction table.)

## 5. Verification

```bash
go build ./... && go test ./... && forge lint
```

Shape + safety checks:

```bash
test -f internal/app/compose.go && grep -rq "func NewComponents" internal/app/compose.go && echo "generated composition present"
grep -rq "func OpenInfra\|type Infra struct" internal/app/providers.go && echo "owned Infra present"
! test -f pkg/app/wire_gen.go && ! test -f pkg/app/services_gen.go && echo "old pkg/app DI unit deleted"
! grep -rq "appkit.Def\|ServiceDef\|appkit.Run\|Options{Only\|BootstrapOnly" pkg/app/ cmd/ && echo "appkit DI table + string selection gone"

# forge map should now flag cycles and narrow-interface mismatches as
# guardrails — run it and confirm a clean report.
forge map
```

The decisive test: a missing required provider must be **loud**, not a nil at
runtime. Temporarily remove a required `Infra` field (so a Deps field resolves
to nothing) and confirm `forge generate` errors or `go build` fails before
merging — that proves the safety property holds.

## 6. Rollback

```bash
git revert <forge-generate-commit>       # undo the regen (restores the old pkg/app DI unit)
git revert <provider-port-commit>        # undo the setup.go → Infra/OpenInfra move
forge upgrade --to <prev-version>        # pin back to the prior version
```

`--to <prev-version>` requires the prior forge build on `PATH`
(`go install github.com/reliant-labs/forge/cmd/forge@vX.Y.Z`).

## See also

- `migrations/v0.x-to-serverkit-composed` — the selection half. The cmd layer
  calls `OpenInfra → NewComponents`, then mounts services with the typed
  `(*app.Components).Mount<Svc>` methods (the data-only `app.Inventory` is
  introspection only) and hands the pieces to `serverkit.Server`. Run that
  migration alongside this one.
- `architecture` skill — the owned `internal/app/providers.go` (Infra/OpenInfra)
  + generated `internal/app/compose.go` (NewComponents) composition model that
  replaces the old `setup.go` + `wire_gen.go` pair.
- `contracts` / `service-layer` skills — `Deps` declares collaborator
  *interfaces*; the provider set decides what fills them (real / client / mock).
