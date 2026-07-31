---
name: getting-started
description: Create a new Forge project, add components, and understand the development workflow.
---

# Getting Started with Forge

## Creating a New Project

```bash
forge project new <project-name> --mod <go-module-path>
```

A bare `forge project new` scaffolds **zero services**: binary shell (`cmd/`),
`internal/app` composition root, buf/proto scaffolding, Taskfile/CI/deploy. The
binary is a deployment unit that mounts services, **not** a domain entity, so
forge never invents a `<project>Service` from the binary name. First step after
a bare scaffold:

```bash
forge scaffold service <entity>   # name it after a domain entity (item, order, user), not the binary
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--mod` | **required** | Go module path (e.g., `github.com/example/my-project`) |
| `--service <name>` | _(none)_ | Initial Go service(s), each an empty proto stub — repeatable or comma-separated; name after a domain entity, not the binary |
| `--frontend <name>` | _(none)_ | Initial Next.js frontend(s) — repeatable or comma-separated |
| `--path <dir>` | `.` | Parent directory for the project |
| `--in-place` | `false` | Scaffold into the current directory instead of creating a subdirectory |
| `--go-version` | _(detected)_ | Go version for go.mod (e.g., `1.24`) |
| `--license` | `MIT` | License to include (`MIT`, `Apache-2.0`, `BSD-3-Clause`, `none`) |
| `--license-author` | _(git user.name)_ | Copyright holder for LICENSE |
| `--kind` | `service` | `service`, `cli` (no server), or `library` (no `cmd/`) — see **Project shape** below |
| `--harness` | `reliant` | Which AI-harness conventions to scaffold: `claude` also writes `CLAUDE.md`, `cursor` a `.cursorrules`, `codex` an `AGENTS.md`. `.claude/skills/` is written either way |
| `--force` | `false` | Overwrite existing project config |

`forge project new --help` lists the rest (`--disable`, `--binary`, `--buf-plugins`, `--frontend-workspaces`, `--skip-tools`).

### Examples

```bash
forge project new my-app --mod github.com/acme/my-app --service users --service orders --frontend web
forge project new --in-place --mod github.com/acme/my-app   # current dir; no name arg
```

## What Gets Scaffolded

**The application lives under `internal/`**, nested by role — a top-level `handlers/`/`workers/`/`operators/` would falsely imply public API. `pkg/` is the generated substrate the app imports. Top-level is otherwise `cmd/` and `api/`.

```
my-app/
├── cmd/                       # entrypoints ONLY: cobra root + one subcommand per binary
├── api/v1alpha1/              # CRD types — kubebuilder convention, importable by clients
├── proto/services/<svc>/v1/   # Proto definitions (if --service given)
├── proto/config/v1/           # config.proto — typed config, its env vars and defaults
├── internal/                  # the application
│   ├── handlers/<svc>/        #   a service: service.go + handlers_crud.go + *_gen.go in ONE dir
│   ├── workers/<name>/        #   background workers (one dir per worker)
│   ├── operators/<name>/      #   k8s operators (one dir per operator)
│   ├── app/                   #   owned providers.go (Infra/OpenInfra) + generated compose.go (NewComponents)
│   └── db/                    #   entity ORM (appears once you have entities)
├── pkg/                       # generated substrate: app/ (test harness), config/, middleware/
├── gen/                       # generated stubs, TS clients, mocks (never hand-edit)
├── frontends/<name>/          # Next.js app (if --frontend given)
├── db/migrations/             # SQL migrations — THE schema source of truth (empty until entities)
├── db/queries/                # SQL queries
├── deploy/                    # Docker, KCL, observability configs
├── e2e/                       # E2E tests
├── forge.yaml                 # Project-GLOBAL config only (identity, frontends, packages)
├── docker-compose.yml         # Dev infra (Postgres, LGTM, etc.)
├── Taskfile.yml               # Task runner aliases
└── .claude/skills/            # These skills, refreshed on every `forge generate`
```

## The Development Workflow

SQL migrations are the schema truth; service protos are the wire truth. `forge generate` projects both into working infrastructure.

**One way in: author the proto.** Mark entity messages with a leading `// forge:entity` comment, add your custom RPCs, then run **`forge scaffold`** — it births every marked entity (CRUD quintet + owned migration pair) and runs generate, stubbing each custom RPC as an Unimplemented method on `*Service`. `--dry-run` prints the plan; re-run is a clean no-op. `forge scaffold entity <name> --from-proto <svc>` narrows the same birth to one named message, and `forge scaffold rpc` adds a single RPC. `forge run` then **boots the dev app alive**, auto-seeding a fresh DB on first boot.

### Phase 1: Scaffold

```bash
forge project new my-app --mod github.com/acme/my-app --service users --frontend web
cd my-app
```

Forge scaffolds the shell — `internal/handlers/<svc>/`, an empty service proto, the `internal/app` root, the `pkg/` substrate, frontend. No tables or entity code until you add an entity.

### Phase 2: Add your first entity

**Author the proto, mark it, scaffold.** Write the message into the service proto under a leading `// forge:entity` comment, add any custom RPCs, then run `forge scaffold`:

```protobuf
// forge:entity
message User {
  string name = 2;
  string email = 3 [(buf.validate.field).string.email = true];
  bool active = 4;
}
```

```bash
forge scaffold
```

It generates the entity struct + ORM (`internal/db/user_orm.go`), CRUD wiring and pages; the born proto surface and migration are yours. There is no CLI field grammar — enums, `optional` (nullable), `buf.validate` rules, `forge:server-set` / `forge:secret` / `forge:append-only`, and real foreign-key references are all spelled in the message, and `forge scaffold entity user name:string` is refused with the equivalent message printed for you. Full surface → `forge project annotations --kind field` and `db`; markers → `proto`.

**Unsure of any syntax? Draft it and run `forge generate` — the fast, exact oracle.** It fails loudly on a bad migration, a missing import, or a List filter naming no real column.

**Two things you are deciding here, whether or not you notice.** Reachability: every rpc is closed to unauthenticated callers unless it carries `auth_required: false`, which publishes that one rpc (`proto`). And ownership: if the rows belong to someone, the owner is a column you write in this migration, because that is what any later authorization check reads (`db`). Both are cheap now and cost a migration or a proto change later — load `auth` before deciding either.

For non-CRUD RPCs, edit the proto directly and re-run `forge generate` (or `forge scaffold`, above). It rebuilds `gen/` (Go stubs, TS clients, mocks, wiring) and never touches your handlers or business logic.

**protovalidate vendors itself — just import it.** To validate wire fields (email/URL/uuid formats, length and numeric bounds, required), add `import "buf/validate/validate.proto";` and annotate fields with `(buf.validate.field)` constraints — `forge generate` pulls the `buf/validate/*` protos into your `proto/` tree on the first run that imports them, so there is no `buf export` or manual-copy step. **Fixed-length codes take an exact rule, not equal bounds:** `string.len = N` (ISO currency 3, country 2, NPI 10) or `string.const` for one fixed value; `buf lint` rejects `min_len == max_len`. Reserve `min_len`/`max_len` for a genuine range.

### Phase 3: Implement business logic

Write business logic in `internal/handlers/<svc>/`, on the `*Service` methods. A custom RPC is a method on `*Service` working with the wire (pb) types and the `internal/db/` ORM through `s.deps`. CRUD RPCs delegate to `pkg/crud` via the owned shims in `handlers_crud.go`. When an RPC's logic deserves transport-free tests or cross-service reuse, scaffold a standalone package (`forge scaffold package <name>`) and call it via `s.deps`. There is no `contract.go`/`Service`-interface split inside the handler package — the `*Service` methods **are** the handlers.

The dependency graph is the explicit composition: the owned `internal/app/providers.go` (`Infra`/`OpenInfra` — DB pool, NATS, k8s, adapters, bindings) and the generated `internal/app/compose.go` (`NewComponents`, constructing every handler/worker/operator inline). Every `Deps` field is an interface, resolved by type off `infra.<Field>` — see `architecture`.

### Phase 4: Evolve the schema (migrations lead, projections follow)

```bash
forge db migration new add_login_tracking    # creates the migration pair
# write the SQL:
#   db/migrations/00002_add_login_tracking.up.sql   (ALTER TABLE users ADD COLUMN ...)
#   db/migrations/00002_add_login_tracking.down.sql
forge generate                               # re-project the entity struct/ORM
forge db migrate up --dsn "$DATABASE_URL"    # apply against a live database
```

Write plain postgres DDL — forge is postgres-pinned, so anything postgres accepts works (`::type` casts, schema-qualified names, `JSONB`, `TEXT[]`). See `db`.

### Phase 5: Wire and schema evolve independently

Generated conversions map the intersection of wire fields and columns by name — a DB-only column (audit trail, cache) never leaks onto the wire, and a wire-only field never reaches the DB. Add either side freely; the divergence is the design.

## Adding Components

```bash
forge scaffold service <name>              # New Connect RPC service (proto stub + handler dir)
forge scaffold entity <name> --from-proto <svc>  # one DB entity, from its authored proto message
forge scaffold rpc <service> <Name>        # Custom RPC: pb-through stub in rpc_<name>.go — one file per
                                           #   RPC either way; + a proto snippet when it is not in the proto
forge scaffold                        # After proto edits: birth `// forge:entity` entities, then generate
forge scaffold worker <name>               # Background worker (Start/Stop lifecycle)
forge scaffold binary <name>               # Standalone long-running binary (own Deployment)
forge scaffold adapter <name>              # Outbound boundary translator (HTTP client, SDK wrapper)
forge scaffold frontend <name>             # Next.js frontend
forge scaffold webhook <name> --service S  # Webhook endpoint on an existing service
forge scaffold package <name>              # Internal Go package with interface contract
                                           #   (also the shape for a use-case orchestrator — see `interactor`)
```

Every `forge scaffold` runs the generation pipeline. It does **not** record
what the project contains: services, workers, binaries, and operators are
discovered from the code that declares them — the proto descriptor,
`internal/workers/`, `internal/operators/`, `cmd/`. Only frontends and packages
take a `forge.yaml` entry.

## Key Commands

| Command | What it does |
|---------|-------------|
| `forge generate` | Regenerate infrastructure from protos + applied migrations (safe anytime) |
| `forge env up dev` | Full stack: Docker infra + Go services (hot reload) + frontends. Defaults `ENVIRONMENT=development` + dev CORS origins when unset. Auth is enforced in every mode — present a real bearer token (see the `auth` skill) |
| `forge env up <env>` | Build + deploy + host launch + frontend dev — one command, reads `deploy/kcl/<env>/` |
| `task test` / `task test:e2e` | Unit + frontends / E2E (needs a stack from `forge env up dev`); `task test:all` adds integration |
| `forge lint` | Go + proto + frontend linters |
| `forge build` | Binaries + frontends. Docker images only with `--docker` (or `--push <registry>`) |
| `forge env deploy dev` | Deploy to local k3d (or whatever dev's KCL targets) |
| `forge db seed apply` | Deterministic, FK-coherent dev data from the applied schema (dev only) |
| `forge run` | Host services + frontends (alias for `forge env up --host-only`); auto-seeds a fresh dev DB on first boot |

## Project shape via `--kind`

Each kind sets its own default `features:` block in forge.yaml: `service` enables
every feature; `cli` enables build/ci/docs and disables
deploy/frontend/observability/codegen; `library` enables docs/contracts and
disables the rest. Override per flag (`features.deploy: true` on a CLI); a
disabled command errors with how to enable it.

## Ports

There is no per-service port: one binary serves every service on one mux, and
that listener is `AppConfig.port` in `proto/config/v1/config.proto` (env var
`PORT`, default `8080`), overridden per env in `deploy/kcl/<env>/config.k`.

In the dev loop (`forge run`, `forge env up`) the backend and every frontend get
a **free port the kernel picks at launch**, so two stacks coexist on one host;
the launch banner prints each one. `--port` on `forge scaffold frontend` pins
one into `forge.yaml` under `frontends[]`. If a postgres already runs on the
host's 5432, `forge env up dev` fails fast before `docker compose up` and prints
the exact `POSTGRES_PORT=<free> forge env up dev` rerun command.

## Rules

- Always run `forge generate` after any proto or migration change. It is always safe: it only touches infrastructure (including `internal/db/<entity>_orm.go`), never business logic or migrations.
- Never hand-edit `gen/` or any `*_gen.go` file. `internal/app/providers.go` (`Infra`/`OpenInfra`) is **owned code you wire**; `internal/app/compose.go` (`NewComponents`) is forge-owned and regenerated every run — `forge project disown internal/app/compose.go` to hand-own it.
- Use `forge scaffold` to scaffold — never copy-paste existing directories.
- Uncertain proto/migration/annotation syntax: **draft it, run `forge generate`, read the error, fix.** Never reverse-engineer forge's `internal/**` source for syntax.
- Use `task test`, not raw `go test` — `Taskfile.yml` sets the tags, and CI runs that same target.
- One service per proto package; its owned and generated files co-locate in one `internal/handlers/<svc>/`.
- DB schema changes go through migrations, not proto edits; the ORM follows the schema.
