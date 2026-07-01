---
name: architecture
description: Forge project conventions and architecture — project structure, generated vs hand-written code, the generate pipeline, proto annotations, contracts, wiring, and naming.
---

# Forge Architecture & Conventions

Forge is a **production infrastructure generator**. It gives you middleware, mocks, observability, test harness, CI/CD, and wiring — the stuff that's tedious to write and easy to get wrong. You own all business logic and the database schema. Forge never touches those.

**Two canonical inputs, two truths:**

- **Proto is the wire truth.** API contracts and config are defined in proto files annotated with `forge.v1` extensions (`method`, `service`, `config`).
- **SQL is the schema truth.** `db/migrations/*.up.sql` is the single source of truth for tables and columns. `forge generate` applies the migrations to an in-memory shadow database, introspects it, and projects entity structs, the ORM, CRUD wiring, and frontend pages from the real tables. There is no entity annotation — the legacy `(forge.v1.entity)` / `(forge.v1.field)` options are retired and ignored.

An **entity** exists when both halves exist: a service proto declares the CRUD RPCs (`Create<X>`/`Get<X>`/`Update<X>`/`Delete<X>`/`List<Xs>`) AND the applied schema has the matching table (pluralized snake_case). `forge add entity` scaffolds both halves in one step.

Two lifecycles drive everything:

- **Scaffold** (runs once per entity): `forge add entity` → create-table migration + CRUD messages/RPCs in the service proto (both yours afterwards)
- **Generate** (runs always): service protos + applied schema → stubs, ORM, hooks, mocks (idempotent)

## Project Structure

> **This is the canonical shape forge generates today.** Older docs and existing trees may still show top-level `handlers/`, `workers/`, `operators/`, `pkg/app/{bootstrap,setup,wire_gen}.go`, a string-keyed service registry, and `serverkit.Run(..., names)`. That is the deprecated layout — the shape below is what `forge new` / `forge add` actually scaffold.

The principle: **express the application as explicit, owned, typed Go — not as a string-keyed registry projected through a clever framework.** App code lives under `internal/`. Top-level is reserved for `cmd/` (entrypoints) and `api/` (genuinely-external CRD types). `pkg/` is ONLY for code with real external importers you support as public API; today there are none, so nothing app-internal lives there.

Components nest under a role-named subtree of `internal/`: services go in `internal/handlers/<svc>/`, workers in `internal/workers/<name>/`, operators in `internal/operators/<name>/`. The role subtree (`handlers`/`workers`/`operators`) lives under `internal/` — not top-level — so nothing app-internal advertises a public API. Within each component's directory the owned and generated files are co-located (one service = one `internal/handlers/<svc>/` dir holding contract.go + impl + handlers_gen.go); generated service mocks land in the shared `internal/handlers/mocks/` directory (package `mocks`, one `<svc>_mock.go` per service).

```
cmd/                      # entrypoints ONLY: one cobra root + real per-command subcommands
  main.go                 #   root
  <svc>.go / <binary>.go  #   one real subcommand per server/binary (owned, ~5 lines via serverCmd helper)
api/v1alpha1/             # CRD types — genuinely external (kubebuilder convention, imported by clients)
internal/                 # DEFAULT HOME for everything not imported outside the module
  handlers/<svc>/         #   a service: contract.go + impl + handlers_gen.go co-located in ONE dir
  workers/<name>/         #   workers live under internal/workers/, NOT top-level workers/
  operators/<name>/       #   operators live under internal/operators/, NOT top-level operators/
  app/                    #   composition: owned providers.go + generated compose.go — was pkg/app
  config/                 #   typed config — was pkg/config
  middleware/             #   thin policy file — was pkg/middleware
  db/                     #   db layer (unchanged)
gen/                      # all generated stubs/clients/mocks
proto/ db/ deploy/ e2e/   # unchanged
```

Per-directory detail:

```
cmd/                          # one cobra root + one real owned subcommand per binary
internal/handlers/<svc>/      # service: contract + impl + generated handlers, co-located
  contract.go                 #   Service interface, Deps struct, New(Deps) (yours)
  <svc>.go                    #   business-logic implementation (yours)
  handlers_gen.go             #   generated Connect handlers — do not edit
  authorizer_gen.go           #   generated RBAC — do not edit
  authorizer.go               #   custom authorization (yours)
  handlers_crud.go            #   thin CRUD RPC delegations (yours — scaffolded once, RPCs appended)
  handlers_crud_test.go       #   CRUD lifecycle test (yours — scaffolded once)
internal/handlers/mocks/      # generated cross-service mocks
internal/workers/<name>/      # workers: worker.go + worker_test.go (one dir per worker)
internal/operators/<name>/    # operators: controller + types (one dir per operator)
internal/app/                 # explicit composition — owned providers seam + generated construction site
  providers.go                #   Infra + OpenInfra(ctx, cfg, logger) — yours (the owned provider set)
  compose.go                  #   Components + NewComponents(infra) — forge-owned, regenerated (disown to hand-own)
  testing.go                  #   integration-test harness
internal/config/              # generated typed config (proto-driven) — one config for ALL binary kinds
internal/middleware/          # thin auth-policy file (yours) + auth_gen.go/tenant_gen.go (generated)
internal/db/                  # database layer
  <entity>_orm.go             #   generated entity struct + ORM, projected from the applied schema
  <entity>_queries.go         #   custom queries (yours — sibling files never touched)
api/v1alpha1/                 # CRD types (genuinely external)
proto/services/<svc>/v1/      # protobuf service definitions (API contracts)
gen/                          # ALL generated code — never hand-edit
  go/ ts/                     #   Go stubs, TypeScript clients
frontends/<name>/             # Next.js frontends (src/app, src/hooks, src/lib)
db/migrations/                # SQL migrations — THE schema source of truth
db/queries/                   # SQL query definitions
deploy/kcl/<env>/             # KCL deployment manifests per environment (+ per-env config)
e2e/                          # end-to-end tests
forge.yaml                    # top-level project config: identity, features, deploy provider
forge_descriptor.json         # proto descriptor data (generated)
```

Why not top-level `handlers/`, `workers/`, `operators/`, or `pkg/app`: a top-level directory falsely advertises public API in an about-to-be-open-sourced repo. These are imported by nobody external, so the role subtree is nested under `internal/` (`internal/handlers/<svc>/`, `internal/workers/<name>/`, `internal/operators/<name>/`). The old `handlers/<svc>/ → internal/<svc>/` two-tier split (6–7 files across 2 dirs, ~25% pure shim forwarders) is gone — generated and owned files are co-located in one component directory. (Forge's own shipped libraries under `forge/pkg/*` are a different module — genuine library code, and they stay.)

## Generated vs Hand-Written

| Forge generates (safe to regenerate) | You own (Forge never touches) |
|--------------------------------------|-------------------------------|
| `gen/` — Go stubs, TS clients, mocks | `internal/handlers/<svc>/<svc>.go` + `contract.go` — business logic |
| `internal/handlers/<svc>/handlers_gen.go` — Connect handlers | `internal/app/providers.go` — the owned `Infra`/`OpenInfra` seam (generated `compose.go` builds off it; `forge disown internal/app/compose.go` to hand-own the construction site) |
| `internal/app/compose.go` — generated `Components`/`NewComponents(infra)` (forge-owned, regenerated every run) | `internal/db/` — mappers, custom queries, schema |
| `internal/middleware/auth_gen.go`, `tenant_gen.go` — auth/tenant codegen | `internal/handlers/<svc>/authorizer.go` — custom auth |
| `internal/handlers/<svc>/authorizer_gen.go` — generated RBAC | `internal/handlers/<svc>/handlers_crud.go` — thin CRUD delegations (scaffolded once, appended-to for new RPCs) |
| `internal/db/<entity>_orm.go` — entity struct + ORM (projected from the applied schema) | `db/migrations/` — schema source of truth |
| Frontend hooks (`*-hooks.ts`) | `db/queries/` — SQL queries |
| `forge_descriptor.json` | `cmd/<binary>.go` — owned per-command subcommands |
| `frontends/<name>/src/lib/connect.ts` | — |

**Rule of thumb**: If it has `_gen` in the name or lives in `gen/`, it's regenerated. Everything else is yours.

### Three precise classes (the unambiguous version)

| Class | Signal | Behaviour |
|-------|--------|-----------|
| **Pure forge-space** | `// Code generated by forge. DO NOT EDIT.` + `// forge-owned: regenerated every run — do not edit (forge disown to take ownership)` header pair, plus an embedded `// forge:hash=<sha256>` self-certification marker (usually a `_gen` filename, but also `internal/db/*_orm.go` and `internal/db/types.go`) + `// Source: …` pointer | Regenerated on every `forge generate`. The embedded hash is the pristine-check: it travels WITH the file through clones, branches, and worktrees, so the drift guard catches hand-edits anywhere — edits abort regeneration until you move them to an extension point, pass `--force`, or take the one-way door (`forge disown <path> --reason ...`). |
| **Scaffold-with-placeholders** | `_gen` filename + body contains `// FORGE_SCAFFOLD: <what to do>` | Regenerated as long as any marker remains. The moment every `FORGE_SCAFFOLD:` line is removed the file becomes user-owned and forge stops overwriting it. The retired CRUD marker-test pair (`handlers_crud_gen_test.go` / `handlers_crud_integration_test.go`) used this tier; its replacement, `handlers_crud_test.go`, is pure user-space (scaffolded once). No current generator output uses this tier, but the semantics still apply to any marker file left on disk. |
| **Pure user-space** | `// yours: scaffolded once, never touched again — forge will not overwrite this file` header | Scaffolded once, never overwritten. The body may contain `// FORGE_SCAFFOLD: …` placeholder TODO markers (e.g. a `handlers_crud.go` "fill in your CRUD delegation" stub) flagging the user's job — markers are in-body TODOs, never ownership banners. |

`forge lint --scaffolds` enforces this:

- A `FORGE_SCAFFOLD:` marker in committed code is a build-gating error
  (`scaffold-not-customized`) — either fill in the placeholder and remove
  the marker, or remove the file from the commit.
- An `_gen.go` file missing the canonical `// Code generated by forge. DO
  NOT EDIT.` header is a build-gating error (`gen-missing-header`).
- An `_gen.go` file missing the `// Source: …` pointer is a warning
  (`gen-missing-source`).

The analyzer runs alongside the others when you invoke `forge lint`,
or in isolation via `forge lint --scaffolds`.

## The Generate Pipeline

```
proto/services/<svc>/v1/<svc>.proto
  → protoc-gen-forge --mode=descriptor → forge_descriptor.json
  → protoc-gen-go + protoc-gen-connect-go → gen/ stubs

db/migrations/*.up.sql (applied to a real ephemeral postgres shadow DB, introspected)
  + service-proto CRUD RPC shapes
  → forge generate → internal/db/<entity>_orm.go (entity struct + ORM)
                     internal/handlers/<svc>/handlers_crud_ops_gen.go (per-RPC ops + ToProto/FromProto conversions)
                     frontend pages, nav, mocks

internal/handlers/<svc>/contract.go (Go interfaces)
  → forge generate → internal/handlers/mocks/<svc>_mock.go

forge_descriptor.json + internal/handlers/<svc>/
  → forge generate → internal/handlers/<svc>/handlers_gen.go, authorizer_gen.go
                     (+ scaffold-once handlers_crud.go / handlers_crud_test.go)

# Observability (logging, tracing, metrics, recovery, request-id) lives in
# forge/pkg/observe as Connect interceptors composed via observe.Chain(observe.Deps{…})
# on the cmd serve path (cmd/<bin>/cmd/serve.go) — not as per-package _gen.go
# wrappers. Pre-1.7 middleware_gen.go/tracing_gen.go/metrics_gen.go files have been removed.

gen/ts/ (TypeScript clients)
  → forge generate → frontends/<name>/src/hooks/*-hooks.ts
```

Running `forge generate` is always safe. It only touches infrastructure — never your handlers, migrations, or business logic. In the DB layer it rewrites only the generated entity ORM files (`internal/db/<entity>_orm.go`); the migrations that drive them are yours.

## Proto annotations (forge.v1)

All annotations use the `forge.v1` package (imported via `forge/v1/forge.proto`):

| Annotation | Applies to | Purpose |
|-----------|-----------|---------|
| `forge.v1.service` | services | Name, version, visibility, auth config |
| `forge.v1.method` | RPCs | auth_required, idempotent, timeout, idempotency_key |
| `forge.v1.config` | fields | env_var, flag, default_value, required, description |

The legacy `(forge.v1.entity)` / `(forge.v1.field)` annotations are **retired and ignored** — their definitions remain in `forge/v1/forge.proto` only as deprecated tombstones, and `forge generate` prints a notice for any message still carrying them. Schema semantics (soft delete, timestamps, tenant scoping) are read off real columns in the applied schema instead — see `db`.

See `proto` for the annotation reference and CRUD naming conventions.

## Component config blocks

Components (services, workers, operators) declare their own typed
configuration as a **config block**: a message in
`proto/config/v1/config.proto`, conventionally named `<Component>Config`,
composed as a field on `AppConfig`. One typed config — generated into
`internal/config` — serves **server, CLI, and standalone-binary** kinds
alike (via the cmdkit paved path); non-server shapes do NOT hand-roll
`os.Getenv`, ad-hoc loggers, or hardcoded timeouts. Scalar Deps fields
(string/int/bool/duration) are the antipattern this replaces — scalars
are configuration, not collaborators, so they belong in a `<Component>Config`
block consumed as one typed `Cfg` field, never as naked scalar Deps fields.

```proto
message TraderConfig {
  int32 max_per_tick = 1 [(forge.v1.config) = {
    env_var: "TRADER_MAX_PER_TICK", default_value: "10",
    description: "Maximum persists per tick"
  }];
}

message AppConfig {
  // ... existing fields ...
  TraderConfig trader = 21;  // no annotation needed on this field
}
```

The component takes the generated block as one typed Deps field:

```go
type Deps struct {
    Logger *slog.Logger
    Cfg    config.TraderConfig  // or *config.TraderConfig
}
```

`forge generate` then:

- emits `type TraderConfig struct {...}` + `Trader TraderConfig` on
  `Config` in `internal/config/config.go`, with env/flag/default loading
  for every leaf and the `.env.example` entries;
- `NewComponents` (in the generated `internal/app/compose.go`) passes the
  block to the component **by type** — `trader.New(trader.Deps{Cfg: cfg.Trader})`.
  Resolution is structural/compile-time: the typed field either matches
  or it does not compile. There is no name-matched wiring layer.
- projects per-env values into `deploy/kcl/<env>/` — KCL is the per-env
  config surface, so `max_per_tick: 50` lives directly where the rest of
  the env lives (logging, env vars), and `forge up --env=dev` injects it
  locally. The old generate-time-only `config.<env>.yaml` files are gutted.

`forge lint --config-deps` (also in `forge audit` as the `config_deps`
category) flags naked-scalar Deps fields with a paste-ready block
snippet. There is no scaffolding command — the two-step is: add the
block message + `AppConfig` field to the config proto, switch the Deps
field to `Cfg config.<Component>Config`, and run `forge generate`.

## Contracts at every boundary

| Boundary | Defined by | Enforced by |
|----------|-----------|-------------|
| **External API** | Proto (`.proto` files) with `forge.v1` annotations | Generated Connect stubs |
| **Internal packages** | Go interfaces (`internal/handlers/<svc>/contract.go`) | Compile-time + contract linter |
| **DI / wiring** | Interface-typed `Deps`, resolved by type in `internal/app/compose.go` (`NewComponents`, off the owned `providers.go` `Infra`/`OpenInfra`) | Compile-time (no name-matched lookup) |
| **Database schema** | SQL migrations (`db/migrations/`) | Postgres at runtime |

Contract enforcement is strict by default — every `internal/` package with exported methods must have a `contract.go` file. Configure exceptions in `forge.yaml`:
```yaml
contracts:
  strict: true
  exclude: ["internal/buildinfo"]
```

## Normalize for the author, denormalize for the machine (KCL ⇄ render)

The deploy layer has two shapes on purpose. They are NOT redundant — each is tuned for a different reader:

- **KCL is the author surface, and it is NORMALIZED.** You declare each object once and reference it by name; KCL computes everything derived. State a fact in exactly one place, let object references carry it everywhere else (DRY, single source of truth). This is the shape a human — or an LLM — writes.
- **The rendered contract (the JSON `forge.render(bundle)` produces, which the deploy machinery consumes) is DENORMALIZED.** The renderer flattens and expands every reference into the concrete strings, paths, and labels the machine needs, duplicating a value wherever the machine wants it inlined. This is the shape a program reads.

**The principle: normalize for the human/LLM author, denormalize for the machine.** You never hand-write the denormalized form. The renderer owns the expansion, so derived values *cannot drift* — a flat string in the JSON can never disagree with the object reference it came from, because nothing typed it twice. This is *why* a KCL field takes an object reference while the JSON it produces is a flat string or label: the reference is the author's single source of truth; the flat value is the renderer's mechanical projection of it.

**Worked example (object reference in → flat strings/labels out).** A secondary cluster names its owner once, by reference:

```python
# KCL — normalized: the owner edge is stated exactly once.
db = Cluster { name = "db" }
api = Cluster { name = "api", owner = db }   # one reference
```

That single `owner = db` reference denormalizes — without the author writing any of it — into the concrete values the deploy machinery consumes:

```json
{ "name": "api", "context": "k3d-api", "network": "k3d-db", "registry_inherit": true }
```

`network` is `k3d-db` *because* it was derived from `owner.name`; it can never name a network the owner doesn't actually have. Had the author instead hand-written the stringly pair (`network = "k3d-db"`, `registry_mirror = "inherit"`), the two could silently disagree with the real owner. The reference makes the wiring unforgeable.

A richer case is a workload's cross-cluster client. One `ClusterClient { cluster, in_cluster, mount_path }` declaration denormalizes into three correlated emissions — a kubeconfig Secret, a Volume that mounts it, and a `CLUSTER_CONFIGS` JSON entry whose `kubeconfigPath` and `context` are computed from the *same* `mount_path` + key the Volume uses. Hand-correlating those three (as you would without the schema) is exactly the drift the normalized declaration eliminates.

So: when a KCL field looks like it "could just be a string," ask whether the string is *derived*. If it is, take the reference and let the renderer denormalize it — that is the contract that keeps the machine-facing JSON honest.

## Database architecture

**Migrations are the source of truth for schema.** Not proto, not Go structs — the SQL in `db/migrations/`. `forge generate` shadow-applies the migrations, introspects the result, and projects the entity struct (`time.Time` for timestamps, pointers for nullable columns, native slices for arrays) plus the ORM into `internal/db/<entity>_orm.go`.

Behavior is read off real columns, no annotations: `deleted_at` ⇒ soft delete, `created_at`+`updated_at` ⇒ managed timestamps, `tenant_id` ⇒ tenant scoping, text columns ⇒ the generated list `search` filter. The wire messages in the service proto evolve independently; generated conversions map the intersection of wire fields and columns by name. See the `db` skill for the full model (type vocabulary, postgres DDL, evolution recipes).

## The composition root (`internal/app/providers.go` + `internal/app/compose.go`)

Wiring is **explicit, typed Go split across an owned provider seam and a generated construction site** — not a generated registration file plus a god-hook. The owned `internal/app/providers.go` declares an `Infra` provider set and `OpenInfra` (DB pool, NATS, k8s client, third-party clients, adapters, explicit interface bindings). The generated `internal/app/compose.go` declares a plain typed `Components` bag and `NewComponents`, which constructs the dependency closure in type-topological order and hands each component its `Deps` as **interface-typed fields, resolved by type — never by string name**:

```go
// internal/app/providers.go (yours — scaffolded once, never regenerated)
type Infra struct {
    Log  *slog.Logger
    Cfg  *config.Config
    DB   *sql.DB
    Repo db.Repository   // explicit interface binding picked here
    LLM  llm.Service
    // ... one field per collaborator a component needs
}

func OpenInfra(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*Infra, error) {
    // open infrastructure, wrap adapters, pick bindings
}

// internal/app/compose.go (forge-owned, regenerated every run —
// `forge disown internal/app/compose.go` to hand-own for two-phase setters)
type Components struct {
    Users user.Service
    Bill  billing.Service
    // ... one field per constructed handler/worker/operator
}

func NewComponents(infra *Infra) (*Components, error) {
    c := &Components{}
    c.Users = user.New(user.Deps{DB: infra.DB})
    c.Bill = billing.New(billing.Deps{
        Users: c.Users,        // interface seam: real in-process instance, resolved BY TYPE off infra/c
        Cfg:   infra.Cfg.Billing,
    })
    // two-phase / late-bound cross-edges (e.g. bill.WithReliantAPIKeyIssuer(c.LLM))
    // are handled by disowning compose.go and editing this construction site by hand.
    return c, nil
}
```

The cmd serve path (`cmd/<bin>/cmd/serve.go`) calls `app.OpenInfra(...)` then `app.NewComponents(infra)`, builds the interceptor chain via `observe.Chain(observe.Deps{Logger, Auth, Audit, RateLimit, Extras})`, applies a typed `Mount<Svc>` method value onto a `serverkit.Server`, and calls `srv.RequireMounted(...)` before `serverkit.Run`.

Key properties:

- **The collaborator interface is the seam.** A component depends on its collaborators' *interfaces* (`Users user.Service`), so it can't tell whether it got the real in-process service, a Connect client, or a mock. In-process is the default fill.
- **Splitting a service out later is a one-line swap in `NewComponents`** — `billing.New(billing.Deps{Users: userclient.New(conn)})` — with the consumer untouched. Cross-binary collaborators get a Connect client; the interface makes the in-process-vs-network boundary explicit.
- **Two-phase wiring is first-class.** Post-construction setters (`bill.WithReliantAPIKeyIssuer(llm)`) and near-diamonds are plain method calls *after* both ends exist — `forge disown internal/app/compose.go` and edit the construction site by hand. Pure constructor topo-ordering deadlocks on the real graph. There is no `PostBuild` hook.
- **Per-binary singletons are plain `Infra` fields.** A collaborator that must be one instance within each of two processes (e.g. `enforcement` in both the server and the workspace-proxy binary) is one field on the owned `Infra`, opened once in `OpenInfra` and shared across the components `NewComponents` builds.
- **NO string-keyed runtime constructor table, NO name-matched wiring.** The old `bootstrap.go` registration, `setup.go` god-hook, and `wire_gen.go` "match Deps by field name/type" layer are removed. Name-matching silently drops narrow-interface mismatches in production (a consumer's narrow `Repository` vs the concrete `*PostgresRepository` field → skipped, nil hazard). The replacement is compile-time: `*db.PostgresRepository` either satisfies `audit.Repository` or it doesn't compile.
- **The registry survives ONLY as a data-only inventory** (`Inventory []inventory.ComponentInfo` in `internal/app/mounts_services.go`) for introspection — `forge map`/`audit`, CLI listing, the cobra mount surface. Names are for *display* only, never a construction lookup key.
- **The payoff: instant real-or-mock instances.** Because every dep is an interface filled in exactly one place, "spin up the app with billing mocked" is a few-line call against `NewComponents` (off a test `Infra`) — no framework, no string lookups, no hidden globals.

DI model choice: the explicit typed composition (`NewComponents` off the owned `Infra`/`OpenInfra` seam) is the default. Runtime typed containers (reflection/generics) are rejected — runtime errors instead of compile errors, can't fill narrow interfaces, awkward two-phase. The `compose.go` codegen is regenerated every run; the moment the graph needs two-phase setters you `forge disown internal/app/compose.go` and the construction site becomes owned Go, which is where the logic ends up anyway.

## Test harness (`internal/app/testing.go`)

`internal/app/testing.go` provides helpers for integration tests — bootstrapping a real app (via `NewComponents` off a test `Infra`) against a test database, with authenticated clients and cleanup:

```go
func TestCreateUser(t *testing.T) {
    harness := app.NewTestHarness(t)
    client := harness.AuthenticatedClient(t, "user-1", "admin")
    // ... test with real DB and middleware
}
```

The harness runs migrations, seeds data, and tears down after each test. Because deps are interfaces filled in one place, swapping a collaborator for a mock fixture is a one-call variation on `NewComponents` (off a test `Infra`).

## Files NOT to Edit

These are regenerated by `forge generate` — your changes will be overwritten:

- `gen/` — All generated Go and TypeScript code
- `internal/middleware/auth_gen.go` / `tenant_gen.go` — auth/tenant shims (the mechanisms live in forge/pkg/{authn,authz,middleware,observe}; `middleware.go` is yours)
- `internal/handlers/<svc>/*_gen.go` — Generated handlers, CRUD op constructors, and authorizers (your CRUD method bodies live in the user-owned `handlers_crud.go`)
- `internal/db/<entity>_orm.go` — Generated entity struct + ORM (projected from the applied `db/migrations/` schema)
- `frontends/<name>/src/hooks/*-hooks.ts` — Generated React Query hooks
- `frontends/<name>/src/lib/connect.ts` — Connect transport setup
- `forge_descriptor.json` — Proto descriptor data

`internal/app/providers.go` and `cmd/<binary>.go` are **yours**; `internal/app/compose.go` is forge-owned and regenerated every run — `forge disown internal/app/compose.go` to take ownership of the construction site (e.g. for two-phase setters). There is no generated string-keyed registration file to avoid editing.

## How pieces connect

1. **Define** API contracts in `proto/` — messages, RPCs, field numbers are forever
2. **Generate** with `forge generate` — fills `gen/` with typed infrastructure and produces `forge_descriptor.json`
3. **Implement** business logic in `internal/handlers/<svc>/<svc>.go` behind the `contract.go` interface
4. **Evolve** DB schema via migrations — `forge generate` re-projects the entity structs/ORM from the applied schema
5. **Consume** from frontends via generated TypeScript Connect clients
6. **Wire** the dependency closure in `internal/app/compose.go` (`NewComponents`, off the owned `providers.go` `Infra`/`OpenInfra`) — typed `Deps`, resolved by type; each `cmd/<binary>.go` is a real owned subcommand
7. **Test** at every level: unit (mocked), integration (real DB), e2e (full stack)

## Key Commands

| Command | When to use |
|---------|------------|
| `forge new <name>` | Create a new project |
| `forge add service <name>` | Add a new Connect RPC service |
| `forge add entity <name> [field:type ...]` | Add a database entity: create-table migration + CRUD proto scaffold |
| `forge add worker <name>` | Add a background worker |
| `forge add frontend <name>` | Add a Next.js frontend |
| `forge generate` | After any proto, migration, or contract change |
| `forge generate --explain` | See per-file provenance: which proto/contract drove each output |
| `forge db migration new <name>` | When you need to change the DB schema |
| `forge db migrate up --dsn $DSN` | Apply pending migrations |
| `forge build` | Verify everything compiles |
| `forge up --env=dev` | Start the full dev stack: infra + Go (hot reload) + Next.js |
| `forge test` | Run all tests (`forge test e2e` for E2E against a running stack) |
| `forge lint` | Go + proto + frontend linters |
| `forge deploy dev` | Deploy to local k3d cluster |
| `forge audit` | Comprehensive project state snapshot (version pin, shape, codegen, packs, scaffolds) |
| `forge audit --json` | Machine-readable audit (sub-agent-friendly) |
| `forge map` | Annotated project tree — every file labelled user-owned vs forge-space |
| `forge map --filter internal/` | Subtree-filtered map |

## Introspection Workflow

When orienting in a forge project (or before making changes):

1. `forge audit` — overall health + drift signals.
2. `forge map --depth 2` — high-level project shape.
3. `forge map --filter <subtree>` — drill into the area you care about.
4. After codegen runs, `forge generate --explain` — see why each file was rewritten.

These commands replace the old "grep for `_gen.go`, eyeball forge.yaml,
hope you didn't miss anything" loop. JSON output everywhere lets sub-agents
chain calls (`forge audit --json | jq '.categories.codegen.details.user_edited_gen_files'`).

## Naming conventions

Forge spans four ecosystems (Go, proto, TS, KCL) with different idiomatic casings, and one identifier (a service / package / entity name) often appears in three forms across them. The rules are mechanical — and `forge add` / `forge generate` do the translation for you — but when you write code or docs that name forge entities, use the right form for the right context:

| Where | Form | Example |
|---|---|---|
| `forge.yaml` service / worker / operator display name | kebab-case | `admin-server`, `git-credential` |
| `forge.yaml` `path:` field (and on-disk directory) | role subtree + lowercase snake_case leaf (`-` → `_`, case boundaries split) | `internal/handlers/admin_server`, `internal/handlers/git_credential` |
| Go package directory under the role subtree | lowercase snake_case leaf | `internal/handlers/admin_server/`, `internal/workers/billing_flow/` |
| Go package declaration | matches the directory — lowercase snake_case identifier | `package admin_server` |
| Go exported type / interface / method | PascalCase | `type Service interface`, `func (s *svc) DoThing(...)` |
| Go local var in the composition root | camelCase | `adminServer`, `billingFlow` |
| Go variable / parameter | camelCase (initialisms stay capitalized) | `orgID`, `createdAt`, `userID` |
| Pack name | kebab-case | `jwt-auth`, `audit-log`, `data-table` |
| Pack subpath under `internal/` | snake / lowercase, valid Go ident | `middleware/auth/jwtauth`, `middleware/audit/auditlog` |
| Proto package | dot-separated lowercase | `myproject.services.users.v1` |
| Proto message / RPC / service | PascalCase | `User`, `CreateUserRequest`, `UserService` |
| Proto field | snake_case | `created_at`, `org_id`, `page_size` |
| Proto enum value | UPPER_SNAKE_CASE prefixed with the enum name | `TASK_STATUS_PENDING` |
| TS component file (under `src/components/ui/`) | snake_case | `data_table.tsx`, `toast_notification.tsx` |
| TS hook / lib / store file | kebab-case | `use-api-query.ts`, `ui-store.ts`, `format-utils.ts` |
| TS component / type export | PascalCase | `DataTable`, `CardHeader` |
| TS hook / variable export | camelCase | `useListUsers`, `pageSize` |
| URL route param / query key | kebab-case | `/audit-events`, `?page-token=...` |

**Same identifier, three forms in one sentence.** When discussing `admin-server`'s `AdminServer.GetUser` RPC handler in `internal/handlers/admin_server/handlers_gen.go` — that's the kebab-case service display name, the PascalCase RPC method, and the canonicalized directory path. All three are intentional and all three are correct. The service / worker / operator canonicalization rule (`strings.ToLower` then strip `-` and `_`) lives in `generator.ServicePackageName`; the `workers` skill Naming section is the source of truth for the on-disk consequences and the migration gotcha.

**Lint enforces the structural ones.** `forgeconv-pk-annotation`, `forgeconv-tenant-annotation`, `forgeconv-one-service-per-file`, `forgeconv-internal-package-contract-names`, and the `--scaffolds` analyzer enforce the proto / contract / scaffold halves of this table mechanically. The Go-style rules (`PascalCase` exports, `camelCase` locals) are enforced by `gofmt` / `goimports` / `staticcheck` already.

This table is the canonical reference. Skills that touch naming heavily — `services`, `migration`, `proto`, `proto-split`, `pack-development`, `frontend`, `api`, `service-layer` — link back here.

## Rules

- Never hand-edit anything under `gen/` or any `*_gen.go` file. Fix the proto or contract, then regenerate.
- App code lives under `internal/`. Top-level is reserved for `cmd/` + `api/`; `pkg/` only for genuinely external public API.
- Wiring is the explicit composition `internal/app/compose.go` (`NewComponents`) off the owned `internal/app/providers.go` `Infra`/`OpenInfra` — `Deps` are interfaces resolved by type. No string-keyed registry, no name-matched wiring; the registry survives only as a data-only inventory for introspection.
- `forge generate` is always safe — it only touches infrastructure, never `providers.go` or `cmd/<binary>.go` (and `compose.go` only while it stays forge-owned; `forge disown` it to hand-own the construction site).
- One service per proto package. One `internal/handlers/<svc>/` directory per service, co-locating contract + impl + generated handlers.
- Field numbers are forever — mark removed fields as `reserved`, never reuse numbers.
- `forge.yaml` tracks ports and services — use `forge add` to scaffold, not copy-paste.
- `forge generate` regenerates the entity ORM (`internal/db/<entity>_orm.go`) from the applied `db/migrations/` schema — the migrations and your query files are yours, and schema truth stays in `db/migrations/`.
- DB schema evolves via migrations, not proto. Proto is for API contracts; the wire messages evolve independently and the generated conversions map the intersection by name.
- An entity needs both halves: CRUD RPCs in a service proto AND the matching table in the applied schema. One without the other generates nothing.
- Contract enforcement is strict by default — configure exceptions in `forge.yaml`.
- Naming follows the canonical table above — kebab-case for display names, snake_case for directories and proto fields, PascalCase for Go exports and proto types, camelCase for Go locals.

## Related skills

Load skills for specific actions: getting-started, services, api, frontend (state, patterns), frontend-testing, proto, db, auth, packs, workers, observability, debug, deploy, testing.
