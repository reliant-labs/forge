---
name: v0.x-to-typed-di
description: Migrate DI from wire_gen.go name-matching + the appkit string registry (Options.Only) to a GENERATED type-topological injector plus an OWNED scaffold-once provider set (internal/app/providers.go) and a PostBuild(*Services) hook for two-phase setters. A missing provider is now a loud compile/build error, not a silent nil. Move setup.go infra construction into the owned provider set; move two-phase setters (e.g. billing.WithReliantAPIKeyIssuer(llm)) into PostBuild. Use when bumping across the typed-DI release.
relevance: migration
---

# Migrating to the typed, type-topological DI injector

Use this skill when `forge upgrade` reports a jump across the release that
replaces name-matched `wire_gen.go` + the `appkit` string registry with a
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
- `appkit` held a string-keyed registry; `Options.Only` selected which
  services to construct by name string.

The actual wiring already lived in a hand-owned `setup.go` (often hundreds of
lines); `wire_gen`'s name-match was a lossy layer on top of it.

**After.** Two clean pieces:

- **A generated type-topological injector** (`internal/app/injector_gen.go` or
  equivalent). It constructs the dependency closure in topological order,
  resolving each `Deps` field **by type, structurally**. There is no string
  lookup and no name-match. If a required provider for some type is absent, it
  is a **loud build error**, not a silent nil — the central safety win.
- **An owned, scaffold-once provider set** (`internal/app/providers.go`). This
  is where you construct infrastructure and the leaf collaborators the injector
  needs (DB pool, NATS, audit sink, adapter-wrapped repos, dialers). forge
  scaffolds it ONCE and never overwrites it — it's yours.
- **A `PostBuild(*Services) error` hook** for two-phase setters. The graph is a
  DAG, but a few wirings are post-construction setters (near-diamonds) — e.g.
  `billing.WithReliantAPIKeyIssuer(llm)`, `authbridge ← billing`. Pure
  constructor topo-ordering deadlocks on those. `PostBuild` runs after every
  service is constructed, so two-phase injection is a plain method call.

```go
// internal/app/providers.go (OWNED — scaffolded once, you maintain it)
func provideRepo(infra Infra) *db.PostgresRepository { return infra.Repo }
func provideEnforcement(repo *db.PostgresRepository) enforcement.Service {
    return enforcement.New(enforcement.Deps{Repo: enforcement.NewDBRepository(repo), /* … */})
}
// … one provider per constructed type. *db.PostgresRepository fills every
// narrow Repository interface a consumer declares — compile-checked.

// internal/app/build.go (GENERATED — type-topological, do not hand-edit)
func Build(infra Infra) (*Services, error) {
    repo := provideRepo(infra)
    enf  := provideEnforcement(repo)
    audit := audit.New(audit.Deps{Repo: repo})        // repo satisfies audit.Repository structurally
    // … topo order resolved by type …
    svcs := &Services{ /* … */ }
    if err := PostBuild(svcs); err != nil { return nil, err }
    return svcs, nil
}

// internal/app/post_build.go (OWNED — two-phase setters live here)
func PostBuild(s *Services) error {
    s.Billing.WithReliantAPIKeyIssuer(s.LLM)          // phase 2, explicit + visible
    s.AuthBridge.SetPlanAssigner(s.Billing)
    return nil
}
```

## 2. Detection

```bash
# Old shape — name-matched wire_gen + appkit string registry.
test -f pkg/app/wire_gen.go && echo "OLD SHAPE — name-matched wire_gen"
grep -rq "Options{Only\|appkit\." pkg/app/ cmd/ && echo "OLD SHAPE — string registry"

# New shape — typed injector + owned provider set.
test -f internal/app/providers.go && echo "owned provider set present"
grep -rq "func PostBuild" internal/app/ && echo "PostBuild hook present"
```

## 3. Migration (deterministic part)

```bash
# 1. Bump forge_version in forge.yaml to the typed-DI release.
# 2. Regenerate. forge DELETES wire_gen.go / the appkit string registry, emits
#    the type-topological injector, and SCAFFOLDS internal/app/providers.go +
#    post_build.go ONCE if they don't exist.
forge generate

# 3. Build — expect errors here on first pass; they are the migration's TODO
#    list (every missing provider is a loud error). See section 4.
go build ./...
```

The regen is intentionally *not* a clean build for non-trivial projects: the
whole point is that previously-silent name-match gaps now surface as compile
errors you must resolve in the owned provider set.

## 4. Migration (manual part)

This is the real work. control-plane/kalshi run this after the forge/pkg bump.

1. **Move setup.go infra construction into the owned provider set.** Your old
   `setup.go` built infrastructure into locals/`*App` fields (DB pool, NATS,
   audit sink, adapter-wrapped repos, dialers nil'd on unset env). Port each
   construction into a `provideXxx` function in `internal/app/providers.go`.
   The injector calls these; the construction logic itself is unchanged — only
   its home moves. Adapter wrapping (`enforcement.NewDBRepository(repo)`) and
   env-conditional dialers go here verbatim.

2. **Move two-phase setters into `PostBuild`.** Any post-construction injection
   — `billing.WithReliantAPIKeyIssuer(llm)`, `authbridge ← billing`, any
   `X.SetY(z)` that ran in setup.go *after* both X and Y existed — becomes a
   plain method call in `PostBuild(*Services)`. The injector guarantees every
   field of `*Services` is populated before `PostBuild` runs.

3. **Fix the narrow-interface fills that name-match used to drop.** Where a
   consumer declares a narrow `Repository` interface and the provider returns
   the concrete `*db.PostgresRepository`, the type system now checks it: either
   the concrete type satisfies the narrow interface (compiles) or it doesn't
   (loud error — fix the interface or the provider). This is the silent-nil
   bug becoming a compile error; resolve each one rather than working around it.

4. **Per-binary singletons.** A collaborator that must be one instance *within
   each* of two binaries (e.g. `enforcement` — one in the server, a separate
   one in workspace-proxy) gets its own provider call in each binary's
   composition root. The provider set can factor a `buildShared(infra)` helper
   both roots reuse; each binary still gets its own instance. "One instance per
   graph" is the natural outcome — don't try to share across processes.

5. **Cross-binary / split-out collaborators.** A binary that should NOT build a
   service in-process fills that collaborator's interface with a Connect client
   instead of the real service — a one-line provider swap
   (`user.Service` ← `userclient.New(conn)`). In-process is the within-binary
   default; the interface seam makes the boundary cheap.

6. **Delete the old artifacts.** Once it builds: confirm `wire_gen.go`, the
   `appkit` string registry usage, and any `Options.Only` / `BootstrapOnly`
   call sites are gone (the serverkit-composed migration removes the selection
   call sites; this one removes the construction table).

## 5. Verification

```bash
go build ./... && go test ./... && forge lint
```

Shape + safety checks:

```bash
test -f internal/app/providers.go && echo "owned provider set present"
grep -rq "func PostBuild" internal/app/ && echo "two-phase setters in PostBuild"
! test -f pkg/app/wire_gen.go && echo "name-matched wire_gen deleted"
! grep -rq "Options{Only\|BootstrapOnly" pkg/app/ cmd/ && echo "string selection gone"

# forge map should now flag cycles and narrow-interface mismatches as
# guardrails — run it and confirm a clean report.
forge map
```

The decisive test: a missing provider must be a **loud build error**, not a
nil at runtime. Temporarily remove a `provideXxx` for a required type and
confirm `go build` fails before merging — that proves the safety property holds.

## 6. Rollback

```bash
git revert <forge-generate-commit>       # undo the regen (restores wire_gen.go)
git revert <provider-port-commit>        # undo the setup.go → providers move
forge upgrade --to <prev-version>        # pin back to the prior version
```

`--to <prev-version>` requires the prior forge build on `PATH`
(`go install github.com/reliant-labs/forge/cmd/forge@vX.Y.Z`).

## See also

- `migrations/v0.x-to-serverkit-composed` — the selection half. Each cobra
  subcommand becomes a composition root that calls `Build(infra)` (or a
  per-binary variant) and hands the result to `serverkit.Server`. Run that
  migration alongside this one.
- `architecture` skill — the owned `internal/app/providers.go` + `PostBuild`
  composition-root model that replaces the old `setup.go` + `wire_gen.go` pair.
- `contracts` / `service-layer` skills — `Deps` declares collaborator
  *interfaces*; the provider set decides what fills them (real / client / mock).
