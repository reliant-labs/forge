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

```
cmd/                          # Application entrypoints (one cobra root)
  main.go                     #   Root command (regenerated)
  server.go                   #   `server [services...]` — serverkit shim (regenerated)
  services_gen.go             #   One subcommand per RegisteredServices row (regenerated)
  commands.go                 #   userCommands() — YOUR extra subcommands (scaffolded once)
proto/services/<svc>/v1/      # Protobuf service definitions (API contracts)
handlers/<svc>/               # Go handler implementations (YOUR business logic)
  service.go                  #   Handler methods
  authorizer.go               #   Custom authorization (yours to edit)
  authorizer_gen.go           #   Generated RBAC (regenerated — do not edit)
  handlers_crud_ops_gen.go    #   Generated per-RPC CRUD op constructors (regenerated — do not edit)
  handlers_crud.go            #   Thin CRUD RPC delegations (YOURS — scaffolded once, new RPCs appended)
  handlers_crud_test.go       #   CRUD lifecycle test (YOURS — scaffolded once)
frontends/<name>/             # Next.js frontends
  src/app/                    #   App Router pages and layouts
  src/hooks/                  #   Generated + custom hooks
  src/lib/                    #   Utilities and Connect client setup
gen/                          # ALL generated code — NEVER hand-edit
  go/                         #   Go stubs (protoc-gen-go, protoc-gen-connect-go)
  ts/                         #   TypeScript clients
internal/db/                  # Database layer
  <entity>_orm.go             #   Generated entity struct + ORM CRUD functions, projected from the applied schema (regenerated)
  <entity>_queries.go         #   Custom queries (YOURS — sibling files are never touched)
internal/<name>/              # Internal Go packages with interface contracts
  contract.go                 #   Interface definition
  <name>.go                   #   Implementation
pkg/app/                      # Application wiring
  bootstrap.go                #   Generated service registration — DO NOT EDIT
  setup.go                    #   Custom wiring — YOUR hook (//forge:allow)
  testing.go                  #   Test harness for integration tests
pkg/middleware/               # Thin auth-policy file (user-owned) + auth_gen.go/tenant_gen.go (generated)
pkg/config/                   # Config struct + loader
db/migrations/                # SQL migrations — THE schema source of truth
db/queries/                   # SQL query definitions
deploy/kcl/<env>/             # KCL deployment manifests per environment
e2e/                          # End-to-end tests
forge.yaml                    # Project config: services, ports, frontends, packs
forge_descriptor.json         # Proto descriptor data (generated)
```

## Generated vs Hand-Written

| Forge generates (safe to regenerate) | You own (Forge never touches) |
|--------------------------------------|-------------------------------|
| `gen/` — Go stubs, TS clients, mocks | `handlers/<svc>/service.go` — business logic |
| `pkg/app/bootstrap.go` — service wiring | `pkg/app/setup.go` — custom wiring |
| `pkg/middleware/auth_gen.go`, `tenant_gen.go` — auth/tenant codegen | `internal/db/` — mappers, custom queries, schema |
| `handlers/<svc>/*_gen.go` — CRUD ops, authorizer | `handlers/<svc>/authorizer.go` — custom auth |
| `internal/db/<entity>_orm.go` — entity struct + ORM (projected from the applied schema) | `handlers/<svc>/handlers_crud.go` — thin CRUD delegations (scaffolded once, appended-to for new RPCs) |
| Frontend hooks (`*-hooks.ts`) | `db/migrations/` — schema source of truth |
| `forge_descriptor.json` | `db/queries/` — SQL queries |
| `frontends/<name>/src/lib/connect.ts` | `internal/<pkg>/` — internal packages |

**Rule of thumb**: If it has `_gen` in the name or lives in `gen/`, it's regenerated. Everything else is yours.

### Three precise classes (the unambiguous version)

| Class | Signal | Behaviour |
|-------|--------|-----------|
| **Pure forge-space** | `// Code generated by forge. DO NOT EDIT.` + `// forge-owned: regenerated every run — do not edit (forge disown to take ownership)` header pair, plus an embedded `// forge:hash=<sha256>` self-certification marker (usually a `_gen` filename, but also `internal/db/*_orm.go` and `internal/db/types.go`) + `// Source: …` pointer | Regenerated on every `forge generate`. The embedded hash is the pristine-check: it travels WITH the file through clones, branches, and worktrees, so the drift guard catches hand-edits anywhere — edits abort regeneration until you move them to an extension point, pass `--force`, or take the one-way door (`forge disown <path> --reason ...`). |
| **Scaffold-with-placeholders** | `_gen` filename + body contains `// FORGE_SCAFFOLD: <what to do>` | Regenerated as long as any marker remains. The moment every `FORGE_SCAFFOLD:` line is removed the file becomes user-owned and forge stops overwriting it. The retired CRUD marker-test pair (`handlers_crud_gen_test.go` / `handlers_crud_integration_test.go`) used this tier; its replacement, `handlers_crud_test.go`, is pure user-space (scaffolded once). No current generator output uses this tier, but the semantics still apply to any marker file left on disk. |
| **Pure user-space** | `// yours: scaffolded once, never touched again — forge will not overwrite this file` header | Scaffolded once, never overwritten. The body may contain `// FORGE_SCAFFOLD: …` placeholder TODO markers (e.g. `setup.go`'s "wire your custom dependencies here") flagging the user's job — markers are in-body TODOs, never ownership banners. |

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
                     handlers_crud_ops_gen.go (per-RPC ops + ToProto/FromProto conversions)
                     frontend pages, nav, mocks

internal/*/contract.go (Go interfaces)
  → forge generate → mock_gen.go

forge_descriptor.json + handlers/<svc>/
  → forge generate → handlers_crud_ops_gen.go, authorizer_gen.go
                     (+ scaffold-once handlers_crud.go / handlers_crud_test.go)

# Observability (logging, tracing, metrics, recovery, request-id) lives in
# forge/pkg/observe as Connect interceptors wired in cmd/server.go — not
# as per-package _gen.go wrappers. Pre-1.7 middleware_gen.go/tracing_gen.go/
# metrics_gen.go files have been removed.

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
composed as a field on `AppConfig`. Scalar Deps fields
(string/int/bool/duration) are the antipattern this replaces — scalars
are configuration, not collaborators, so wire_gen can never resolve them
from App/AppExtras (they regenerate as typed zeros + TODOs forever).

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
  `Config` in `pkg/config/config.go`, with env/flag/default loading for
  every leaf and the `.env.example` entries;
- wires `Cfg: cfg.Trader` in `pkg/app/wire_gen.go` **by type** — exactly
  one `Config` field of the block type matches; two fields of the same
  block type are a hard generate error listing the candidates;
- projects per-env values: `max_per_tick: 50` in `config.<env>.yaml`
  (flat snake_case leaf keys, same namespace as root fields) lands in
  the env's generated ConfigMap + env var like any root field, and
  `forge up --env=dev` injects it locally.

Resolution precedence: an AppExtras field whose NAME matches the Deps
field still wins (explicit user wiring); the type-based config-block
match applies only when nothing else resolved. Keep leaf field names
unique across blocks — the per-env yaml namespace is flat.

`forge lint --config-deps` (also in `forge audit` as the `config_deps`
category) flags naked-scalar Deps fields with a paste-ready block
snippet. There is no scaffolding command — the two-step is: add the
block message + `AppConfig` field to the config proto, switch the Deps
field to `Cfg config.<Component>Config`, and run `forge generate`.

## Contracts at every boundary

| Boundary | Defined by | Enforced by |
|----------|-----------|-------------|
| **External API** | Proto (`.proto` files) with `forge.v1` annotations | Generated Connect stubs |
| **Internal packages** | Go interfaces (`internal/<pkg>/contract.go`) | Compile-time + contract linter |
| **Database schema** | SQL migrations (`db/migrations/`) | Postgres at runtime |

Contract enforcement is strict by default — every `internal/` package with exported methods must have a `contract.go` file. Configure exceptions in `forge.yaml`:
```yaml
contracts:
  strict: true
  exclude: ["internal/buildinfo"]
```

## Database architecture

**Migrations are the source of truth for schema.** Not proto, not Go structs — the SQL in `db/migrations/`. `forge generate` shadow-applies the migrations, introspects the result, and projects the entity struct (`time.Time` for timestamps, pointers for nullable columns, native slices for arrays) plus the ORM into `internal/db/<entity>_orm.go`.

Behavior is read off real columns, no annotations: `deleted_at` ⇒ soft delete, `created_at`+`updated_at` ⇒ managed timestamps, `tenant_id` ⇒ tenant scoping, text columns ⇒ the generated list `search` filter. The wire messages in the service proto evolve independently; generated conversions map the intersection of wire fields and columns by name. See the `db` skill for the full model (type vocabulary, postgres DDL, evolution recipes).

## Custom Wiring in setup.go

`pkg/app/bootstrap.go` is generated and auto-registers services, workers, and internal packages. **Never edit it.**

`pkg/app/setup.go` is yours. Use it to wire custom dependencies — database handles, external clients, feature flags, anything `bootstrap.go` can't know about:

```go
// pkg/app/setup.go
func Setup(app *App) error {
    // Wire custom dependencies here
    app.UserService.DB = app.Pool
    app.UserService.EmailClient = ses.NewClient(app.Config.AWS)
    return nil
}
```

### Decomposing setup.go in worker-heavy projects

`Setup()` in one file is fine up to ~150 LOC. Past that — especially for projects with many workers, each needing its own infrastructure construction — split into sibling files in the same `package app`. Go has no problem with multiple files exporting helpers used by `Setup`:

```
pkg/app/
  setup.go              # Setup() — calls helpers below
  setup_workers.go      # buildWorkerInfra(app) — NATS subscribers, ticker queues
  setup_handlers.go     # buildHandlerInfra(app) — DB pool, ORM, audit sink
  setup_external.go     # buildExternalClients(app) — Stripe, SES, third-party SDKs
```

Each helper builds and assigns to `app.*Extras` fields; `Setup` orchestrates the call order. The convention is owner-driven — forge generates nothing here — but a flat 600-LOC `setup.go` is a code-review hazard. Split early.

`setup.go` is marked with `//forge:allow` and will never be overwritten.

## Test Harness in testing.go

`pkg/app/testing.go` provides helpers for integration tests — bootstrapping a real app with a test database, authenticated clients, and cleanup:

```go
func TestCreateUser(t *testing.T) {
    harness := app.NewTestHarness(t)
    client := harness.AuthenticatedClient(t, "user-1", "admin")
    // ... test with real DB and middleware
}
```

The harness runs migrations, seeds data, and tears down after each test.

## Files NOT to Edit

These are regenerated by `forge generate` — your changes will be overwritten:

- `gen/` — All generated Go and TypeScript code
- `pkg/app/bootstrap.go` — Service registration and wiring
- `pkg/middleware/auth_gen.go` / `tenant_gen.go` — auth/tenant shims (the mechanisms live in forge/pkg/{authn,authz,middleware,observe}; `middleware.go` is yours)
- `handlers/<svc>/*_gen.go` — Generated CRUD op constructors and authorizers (your CRUD method bodies live in the user-owned `handlers_crud.go`)
- `internal/db/<entity>_orm.go` — Generated entity struct + ORM (projected from the applied `db/migrations/` schema)
- `frontends/<name>/src/hooks/*-hooks.ts` — Generated React Query hooks
- `frontends/<name>/src/lib/connect.ts` — Connect transport setup
- `forge_descriptor.json` — Proto descriptor data

## How pieces connect

1. **Define** API contracts in `proto/` — messages, RPCs, field numbers are forever
2. **Generate** with `forge generate` — fills `gen/` with typed infrastructure and produces `forge_descriptor.json`
3. **Implement** handlers in `handlers/<svc>/service.go` — your business logic
4. **Evolve** DB schema via migrations — `forge generate` re-projects the entity structs/ORM from the applied schema
5. **Consume** from frontends via generated TypeScript Connect clients
6. **Wire** custom dependencies in `pkg/app/setup.go` (`bootstrap.go` is generated — do not edit)
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
| `forge map --filter handlers/` | Subtree-filtered map |

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
| `forge.yaml` `path:` field (and on-disk directory) | lowercase snake_case (`-` → `_`, case boundaries split) | `handlers/admin_server`, `handlers/git_credential` |
| Go package directory under `handlers/` / `internal/` | lowercase snake_case | `handlers/admin_server/`, `internal/billing_flow/` |
| Go package declaration | matches the directory — lowercase snake_case identifier | `package admin_server` |
| Go exported type / interface / method | PascalCase | `type Service interface`, `func (s *svc) DoThing(...)` |
| Go exported field on `*App` | PascalCase | `app.AdminServer`, `app.BillingFlow` |
| Go variable / parameter | camelCase (initialisms stay capitalized) | `orgID`, `createdAt`, `userID` |
| Pack name | kebab-case | `jwt-auth`, `audit-log`, `data-table` |
| Pack subpath under `pkg/` | snake / lowercase, valid Go ident | `middleware/auth/jwtauth`, `middleware/audit/auditlog` |
| Proto package | dot-separated lowercase | `myproject.services.users.v1` |
| Proto message / RPC / service | PascalCase | `User`, `CreateUserRequest`, `UserService` |
| Proto field | snake_case | `created_at`, `org_id`, `page_size` |
| Proto enum value | UPPER_SNAKE_CASE prefixed with the enum name | `TASK_STATUS_PENDING` |
| TS component file (under `src/components/ui/`) | snake_case | `data_table.tsx`, `toast_notification.tsx` |
| TS hook / lib / store file | kebab-case | `use-api-query.ts`, `ui-store.ts`, `format-utils.ts` |
| TS component / type export | PascalCase | `DataTable`, `CardHeader` |
| TS hook / variable export | camelCase | `useListUsers`, `pageSize` |
| URL route param / query key | kebab-case | `/audit-events`, `?page-token=...` |

**Same identifier, three forms in one sentence.** When discussing `admin-server`'s `AdminServer.GetUser` RPC handler in `handlers/adminserver/handlers.go` — that's the kebab-case service display name, the PascalCase `*App` field plus the PascalCase RPC method, and the canonicalized directory path. All three are intentional and all three are correct. The service / worker / operator canonicalization rule (`strings.ToLower` then strip `-` and `_`) lives in `generator.ServicePackageName`; the `workers` skill Naming section is the source of truth for the on-disk consequences and the migration gotcha.

**Lint enforces the structural ones.** `forgeconv-pk-annotation`, `forgeconv-tenant-annotation`, `forgeconv-one-service-per-file`, `forgeconv-internal-package-contract-names`, and the `--scaffolds` analyzer enforce the proto / contract / scaffold halves of this table mechanically. The Go-style rules (`PascalCase` exports, `camelCase` locals) are enforced by `gofmt` / `goimports` / `staticcheck` already.

This table is the canonical reference. Skills that touch naming heavily — `services`, `migration`, `proto`, `proto-split`, `pack-development`, `frontend`, `api`, `service-layer` — link back here.

## Rules

- Never hand-edit anything under `gen/` or any `*_gen.go` file. Fix the proto or contract, then regenerate.
- `bootstrap.go` is regenerated — all custom wiring goes in `setup.go`.
- `forge generate` is always safe — it only touches infrastructure.
- One service per proto package. One handler directory per service.
- Field numbers are forever — mark removed fields as `reserved`, never reuse numbers.
- `forge.yaml` tracks ports and services — use `forge add` to scaffold, not copy-paste.
- `forge generate` regenerates the entity ORM (`internal/db/<entity>_orm.go`) from the applied `db/migrations/` schema — the migrations and your query files are yours, and schema truth stays in `db/migrations/`.
- DB schema evolves via migrations, not proto. Proto is for API contracts; the wire messages evolve independently and the generated conversions map the intersection by name.
- An entity needs both halves: CRUD RPCs in a service proto AND the matching table in the applied schema. One without the other generates nothing.
- Contract enforcement is strict by default — configure exceptions in `forge.yaml`.
- Naming follows the canonical table above — kebab-case for display names, snake_case for directories and proto fields, PascalCase for Go exports and proto types, camelCase for Go locals.

## Related skills

Load skills for specific actions: getting-started, services, api, frontend (state, patterns), frontend-testing, proto, db, auth, packs, workers, observability, debug, deploy, testing.
