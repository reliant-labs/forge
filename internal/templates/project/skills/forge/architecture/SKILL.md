---
name: architecture
description: Architecture map — project structure, generated vs hand-written code, the generate pipeline, wiring, files-not-to-edit, and a naming + related-skills index. Deep topic detail lives in the owning skills (proto, db, contracts, service-layer, observability, testing, deploy).
---

# Forge Architecture & Conventions

Forge is a **production infrastructure generator**: middleware, mocks, observability, test harness, CI/CD, and wiring. You own all business logic and the database schema; forge never touches those.

**Two canonical inputs, two truths:**

- **Proto is the wire truth.** API contracts and config live in proto files annotated with `forge.v1` extensions (`method`, `service`, `config`). See `proto`.
- **SQL is the schema truth.** `db/migrations/*.up.sql` is the single source of truth for tables/columns. `forge generate` applies the migrations to an in-memory shadow database, introspects it, and projects entity structs, the ORM, CRUD wiring, and frontend pages. There is no entity annotation; columns come from the applied schema. See `db`.

An **entity** exists when both halves exist: a service proto declares the CRUD RPCs (`Create<X>`/`Get<X>`/`Update<X>`/`Delete<X>`/`List<Xs>`) AND the applied schema has the matching table (pluralized snake_case).

**The fast path (proto-first).** Mark each entity message with a leading `// forge:entity` comment, add custom RPCs, then run `forge scaffold`: it births every marked entity (CRUD quintet + owned create-table migration) and runs generate, stubbing every custom RPC (`--dry-run` plans; a clean re-run is a no-op). Depth lives in the owning skill — entity births/seeds in `db`, RPC handlers in `api`, domain packages in `contracts`/`service-layer`.

## Project Structure

**Principle:** express the app as explicit, owned, typed Go — not a string-keyed registry through a clever framework. The application lives under `internal/`, nested by role (`handlers`/`workers`/`operators`) so owned and generated files are **co-located** in one component directory. `pkg/` is the thin generated substrate the app imports — typed config, the middleware policy file, the test harness. Top-level is otherwise `cmd/` (entrypoints) and `api/` (genuinely-external CRD types).

```
cmd/                          # entrypoints ONLY: cobra root + one owned subcommand per binary (~5 lines each)
api/v1alpha1/                 # CRD types — genuinely external (kubebuilder convention)
internal/                     # the application: components, DI, data access
  handlers/<svc>/             #   a service, all co-located in ONE dir:
    service.go                #     yours: Deps + validateDeps + New + Register
    handlers_crud.go          #     yours: thin CRUD delegations (access-control / row-scoping WHERE goes here)
    rpc_<name>.go             #     yours: ONE custom RPC per file (scaffold rpc + generate agree)
    handlers_crud_test.go     #     yours: CRUD lifecycle test (scaffolded once)
    handlers_crud_ops_gen.go  #     generated: per-RPC CRUD ops + ToProto/FromProto
  handlers/mocks/             #   generated cross-service mocks (package mocks, one <svc>_mock.go each)
  <name>/                     #   standalone domain package: contract.go (yours) + mock_gen.go
  workers/<name>/             #   workers (NOT top-level): worker.go + worker_test.go
  operators/<name>/           #   operators (NOT top-level): controller + types
  app/                        #   the LIVE dependency-injection composition root:
    providers.go              #     yours: Infra + OpenInfra(ctx, cfg, logger) — the owned provider set
    auth.go                   #     yours: SetupAuth() over forge/pkg/auth
    compose.go                #     generated: Components + NewComponents(infra) — disown to hand-own
    mounts_services.go        #     generated: typed Mount<Svc> methods + the data-only Inventory
    lifecycle.go              #     generated: start/stop supervision for workers and operators
  db/                         #   <entity>_orm.go (generated from schema) + <entity>_repo_ext.go (yours)
pkg/app/                      # substrate: testing.go (per-component test harness) + migrate.go
pkg/config/                   # typed config projected from proto/config/v1/config.proto
pkg/middleware/               # thin owned policy file
gen/                          # ALL generated stubs/clients/mocks + forge_descriptor.json — never hand-edit
proto/services/<svc>/v1/      # protobuf service definitions (API contracts)
proto/config/v1/config.proto  # config truth: every field, its env var, flag, and default
db/migrations/                # SQL migrations — THE schema source of truth   (db/queries/ — SQL queries)
frontends/<name>/             # Next.js frontends (src/app, src/hooks, src/lib)
deploy/kcl/<env>/             # KCL deployment manifests + per-env config       (e2e/ — end-to-end tests)
forge.yaml                    # project-GLOBAL config only: identity, frontends, packages, overrides
```

`pkg/app` is substrate only — the live DI composition is `internal/app` (`pkg/app/CONVENTIONS.md` documents the split). There is no top-level `handlers/`/`workers/`/`operators/` — that would falsely advertise public API. **What the project contains is discovered from the code** — service protos, `internal/workers/`, `internal/operators/`, `cmd/` — never from a list in `forge.yaml`.

## Generated vs Hand-Written

| Forge generates (safe to regenerate) | You own (Forge never touches) |
|---|---|
| `gen/` — Go stubs, TS clients, mocks | `internal/handlers/<svc>/service.go`, `rpc_<name>.go` — business logic |
| `internal/handlers/<svc>/handlers_crud_ops_gen.go` | `internal/handlers/<svc>/handlers_crud.go` — CRUD delegations (access-control WHERE) |
| `internal/app/compose.go`, `mounts_services.go`, `lifecycle.go` (disown to hand-own) | `internal/app/providers.go`, `auth.go` — the `Infra`/`OpenInfra`/`SetupAuth` seams |
| `internal/db/<entity>_orm.go` — entity struct + ORM (from schema) | `db/migrations/` — schema truth; `internal/db/<entity>_repo_ext.go` — custom queries |
| `pkg/config/`, `pkg/app/testing.go`, frontend `*-hooks.ts`, `gen/forge_descriptor.json` | `pkg/middleware/` — policy; `cmd/<binary>.go` — owned subcommands |

**Rule of thumb:** `_gen` in the name or under `gen/` — regenerated; `pkg/` is a mix (table above).

**Ownership signals.** Forge-owned files carry `// Code generated by forge. DO NOT EDIT.` + an embedded `// forge:hash=<sha256>` that travels through clones and worktrees, so the drift guard catches hand-edits anywhere — they abort regeneration until you move them to an extension point or take the one-way door (`forge project disown <path> --reason ...`). Yours carry `// yours: scaffolded once`. `forge lint --scaffolds` warns on surviving `// FORGE_SCAFFOLD:` TODO markers (not build-gating) and errors on a `_gen.go` missing its generated header.

## The Generate Pipeline

```
proto/services/<svc>/v1/<svc>.proto
  → protoc-gen-forge → forge_descriptor.json; protoc-gen-{go,connect-go} → gen/ stubs
  → forge generate → a pb-through stub per custom RPC (+ scaffold-once handlers_crud.go / handlers_crud_test.go)

db/migrations/*.up.sql (shadow-applied to a real ephemeral postgres, introspected) + CRUD RPC shapes
  → forge generate → internal/db/<entity>_orm.go, internal/handlers/<svc>/handlers_crud_ops_gen.go, frontend pages/nav/mocks

proto/config/v1/config.proto → forge generate → pkg/config/ + deploy/kcl/config_gen.k
internal/<name>/contract.go (standalone domain packages) → forge generate → internal/<name>/mock_gen.go
gen/ts/ → forge generate → frontends/<name>/src/hooks/*-hooks.ts
```

`forge generate` is always safe — it only touches infrastructure, never your handlers, migrations, or business logic. In the DB layer it rewrites only `internal/db/<entity>_orm.go`. Observability is applied by `forge/pkg/observe` interceptors on the serve path, not per-package `_gen.go` wrappers — see `observability`.

## The composition root (`internal/app/providers.go` + `internal/app/compose.go`)

Wiring is **explicit, typed Go** — no registration file, no god-hook, no string-keyed lookup. The owned `providers.go` declares an `Infra` set + `OpenInfra` (pools, clients, adapters, interface bindings). The generated `compose.go` declares `NewComponents`, which builds the closure in type-topological order and hands each component its `Deps` as **interface-typed fields, resolved by type**:

```go
// internal/app/compose.go (generated; disown to hand-own)
func NewComponents(infra *Infra) (*Components, error) {
    c := &Components{}
    c.Users = user.New(user.Deps{DB: infra.DB})
    c.Bill  = billing.New(billing.Deps{
        Users: c.Users,          // interface seam, resolved BY TYPE
        Cfg:   infra.Cfg.Billing, // typed config, never a naked scalar Dep
    })
    return c, nil
}
```

- **The `Deps` interface is the seam.** A component depends on each dep's *interface* — the field is `Users user.Service`, never the concrete type — so it can't tell the real in-process service from a Connect client or a mock. Splitting a service out later is a one-line swap here (`Users: userclient.New(conn)`), consumer untouched.
- **Resolution is compile-time, by type — never by name.** If `infra.Repo` doesn't satisfy `things.Repository`, it doesn't compile — no name-match layer to silently drop a narrow-interface mismatch as a nil field. Runtime typed containers (reflection/generics) are rejected for exactly this reason.
- **Two-phase wiring is first-class.** Post-construction setters and near-diamonds are plain method calls after both ends exist: `forge project disown internal/app/compose.go` and edit by hand.

The cmd serve path calls `app.OpenInfra(...)` → `app.NewComponents(infra)`, mounts each service through the typed `Mount<Svc>` methods, and calls `serverkit.Run`. The `Inventory` beside them is data-only, for `forge project map`/`audit` — names are display-only, never a construction key.

## Files NOT to Edit

Regenerated (changes overwritten): `gen/`, every `*_gen.go`, `internal/db/<entity>_orm.go`, `pkg/config/`, `pkg/app/`, `frontends/<name>/src/hooks/*-hooks.ts` + `src/lib/connect.ts`, and the three generated `internal/app` files until disowned. Your CRUD bodies live in the owned `handlers_crud.go`; the validator behind `auth.go` is yours to pick — see `auth`.

## Naming conventions

Forge spans four ecosystems (Go, proto, TS, KCL) with different idiomatic casings; one identifier often appears in three forms. `forge scaffold` / `forge generate` translate, but write the right form by hand. **This table is the canonical reference — naming-adjacent skills (`services`, `migration`, `proto`, `proto-split`, `frontend`, `api`, `service-layer`) link back here.**

| Where | Form | Example |
|---|---|---|
| Component display name (KCL, `forge project map`) | kebab-case | `admin-server`, `git-credential` |
| Component directory on disk | role subtree + snake_case leaf | `internal/handlers/admin_server` |
| Go package declaration | snake_case (matches dir) | `package admin_server` |
| Go exported type / interface / method | PascalCase | `type Service interface`, `DoThing(...)` |
| Go local / variable / parameter | camelCase (initialisms stay caps) | `adminServer`, `orgID`, `createdAt` |
| Proto package | dot-separated lowercase, no project prefix | `services.users.v1` |
| Proto message / RPC / service | PascalCase | `User`, `CreateUserRequest`, `UserService` |
| Proto field | snake_case | `created_at`, `org_id`, `page_size` |
| Proto enum value | UPPER_SNAKE_CASE, enum-name prefixed | `TASK_STATUS_PENDING` |
| TS component file (`src/components/ui/`) | snake_case | `data_table.tsx`, `toast_notification.tsx` |
| TS hook / lib / store file | kebab-case | `use-api-query.ts`, `ui-store.ts` |
| TS component / type export | PascalCase | `DataTable`, `CardHeader` |
| TS hook / variable export | camelCase | `useListUsers`, `pageSize` |
| URL route param / query key | kebab-case | `/audit-events`, `?page-token=...` |

Lint enforces the structural halves (`forgeconv-one-service-per-file`, `forgeconv-internal-package-contract-names`, `--scaffolds`); `gofmt`/`goimports`/`staticcheck` enforce the Go-style rules.

## Deep detail lives in the owning skill

For the annotation vocabulary — every `// forge:*` marker, the proto→column mapping a birth applies, the projected `buf.validate` rules, and the `(forge.v1.service)`/`(forge.v1.method)` options — run **`forge project annotations --json`**: emitted from forge's own descriptors, so it cannot drift.

This skill is the map. For depth, load: **proto** (annotations, CRUD naming, field rules); **db** (migrations, entity types, schema evolution, ORM, op-overrides); **contracts** (contracts at every boundary, `contract.go`, strict mode); **services** (`<Component>Config` config blocks → `pkg/config` + per-env KCL); **service-layer** / **api** (interfaces-at-the-consumer, structs-out, role interfaces); **observability** (interceptor chain, per-method decorator, dashboards); **testing** (`pkg/app/testing.go`, real-DB integration, mocks); **deploy** (KCL normalize-for-the-author ⇄ denormalize-for-the-machine, `forge.render`).

## Rules

- Never hand-edit anything under `gen/` or any `*_gen.go`. Fix the proto or contract, then regenerate.
- The app lives under `internal/`, by role; `pkg/` is generated substrate (config, middleware, test harness); top-level is otherwise `cmd/` + `api/`.
- Wiring is the explicit composition (`NewComponents`) off the owned `providers.go` `Infra`/`OpenInfra` — `Deps` are interfaces resolved by type, never by name.
- `forge generate` never touches `providers.go`/`auth.go`/`cmd/<binary>.go` (and `compose.go` only while forge-owned).
- One service per proto package; one `internal/handlers/<svc>/` per service. Field numbers are forever — mark removed fields `reserved`.
- DB schema evolves via migrations, not proto; wire messages evolve independently and conversions map the intersection by name. An entity needs both halves.

## Related skills

Also: forge, frontend (state, patterns), frontend-testing, auth, workers, debug.
