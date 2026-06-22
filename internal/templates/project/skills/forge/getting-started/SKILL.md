---
name: getting-started
description: Create a new Forge project, add components, and understand the development workflow.
---

# Getting Started with Forge

## Creating a New Project

```bash
forge new <project-name> --mod <go-module-path>
```

A bare `forge new` scaffolds **zero services**: binary shell (`cmd/`),
`internal/app` composition root, buf/proto scaffolding, Taskfile/CI/deploy — and
`services: []` in forge.yaml. The binary is a deployment unit that
mounts services; it is **not** a domain entity, so forge never invents
a `<project>Service` from the binary name. The first step after a bare
scaffold is:

```bash
forge add service <entity>   # name it after a domain entity (item, order, user), not the binary
```

Pass `--service <entity>` at creation time to opt into an initial
service (an empty service proto stub — add RPCs by hand or via
`forge add entity`, which scaffolds the CRUD messages and RPCs for you).

### Required flags

| Flag | Description |
|------|-------------|
| `--mod` | Go module path (e.g., `github.com/example/my-project`) — **required** |

### Optional flags

| Flag | Default | Description |
|------|---------|-------------|
| `--service <name>` | _(none — zero services)_ | Initial Go service(s) — repeatable or comma-separated; name after a domain entity, not the binary |
| `--frontend <name>` | _(none)_ | Initial Next.js frontend(s) — repeatable or comma-separated |
| `--path <dir>` | `.` | Parent directory for the project |
| `--in-place` | `false` | Scaffold into the current directory instead of creating a subdirectory |
| `--go-version` | _(detected)_ | Go version for go.mod (e.g., `1.24`) |
| `--license` | `MIT` | License to include (`MIT`, `Apache-2.0`, `BSD-3-Clause`, `none`) |
| `--license-author` | _(git user.name)_ | Copyright holder for LICENSE |
| `--force` | `false` | Overwrite existing project config |

### Examples

```bash
# Minimal — creates project dir with ZERO services; follow with `forge add service <entity>`
forge new my-app --mod github.com/acme/my-app

# With service and frontend
forge new my-app --mod github.com/acme/my-app --service gateway --frontend web

# Multiple services
forge new my-app --mod github.com/acme/my-app --service users --service orders

# In existing directory
forge new --in-place --mod github.com/acme/my-app
```

## What Gets Scaffolded

Top-level is reserved for `cmd/` (entrypoints) and `api/` (CRD types — the only genuinely-external code). **Everything else lives under `internal/`** — services, workers, operators, app wiring, config, middleware. Components nest under a role subtree of `internal/` (`internal/handlers/<svc>/`, `internal/workers/<name>/`, `internal/operators/<name>/`); there is no top-level `handlers/`, `workers/`, `operators/`, or `pkg/app`, since a top-level dir would falsely imply public API. `pkg/` is reserved for code with real external importers (today there are none).

```
my-app/
├── cmd/                       # entrypoints ONLY: cobra root + one subcommand per binary
├── api/v1alpha1/              # CRD types (if any) — kubebuilder convention, importable by clients
├── proto/services/<svc>/v1/   # Proto definitions (if --service given)
├── internal/                  # default home for everything not imported outside the module
│   ├── handlers/<svc>/        #   a service: contract.go + impl + handlers_gen.go in ONE dir
│   ├── workers/<name>/        #   background workers (one dir per worker)
│   ├── operators/<name>/      #   k8s operators (one dir per operator)
│   ├── app/                   #   composition: owned providers.go (Infra/OpenInfra) + generated compose.go (NewComponents) — the wiring you own
│   ├── config/                #   typed config (proto-driven)
│   ├── middleware/            #   thin auth/logging/tenant policy
│   └── db/                    #   entity ORM (appears once you have entities)
├── gen/                       # generated stubs, TS clients, mocks (never hand-edit)
├── frontends/<name>/          # Next.js app (if --frontend given)
├── db/migrations/             # SQL migrations — THE schema source of truth (empty until you add entities)
├── db/queries/                # SQL query directory
├── deploy/                    # Docker, KCL, observability configs
├── e2e/                       # E2E test directory
├── forge.yaml                 # Project config (strictly top-level: identity, features, deploy)
├── docker-compose.yml         # Dev infra (Postgres, LGTM, etc.)
├── Taskfile.yml               # Task runner aliases
└── .reliant/skills/forge/     # These skills
```

## The Development Workflow

SQL migrations are the schema truth; service protos are the wire truth. `forge generate` projects both into working infrastructure.

### Phase 1: Scaffold

```bash
forge new my-app --mod github.com/acme/my-app --service users --frontend web
cd my-app
```

Forge scaffolds the shell — an `internal/handlers/<svc>/` package, an empty service proto, middleware, the `internal/app` composition root, and frontend. There are no tables (and no entity code) until you add an entity.

### Phase 2: Add your first entity

```bash
forge add entity user name:string email:string active:bool
forge generate
```

`forge add entity` writes the create-table migration into `db/migrations/` and (once) scaffolds the CRUD messages + RPCs into the service proto. `forge generate` then applies the migrations to an in-memory shadow DB, introspects the schema, and projects the entity struct + ORM (`internal/db/user_orm.go`), CRUD wiring, and frontend pages. Both scaffolded halves — the migration and the proto — are yours afterwards.

For non-CRUD RPCs, edit the proto directly and re-run `forge generate`. It rebuilds `gen/` (Go stubs, TS clients, mocks, wiring) and never touches your handlers or business logic.

### Phase 3: Implement business logic

Write your business logic in `internal/handlers/<svc>/` — `contract.go` declares the `Service` interface + `Deps`, and the implementation lives behind it. The handler (`handlers_gen.go`, co-located in the same dir) is thin translation that calls into the service. Use the generated types and the `internal/db/` ORM functions.

The server's dependency graph is wired in the explicit composition: the owned `internal/app/providers.go` (`Infra`/`OpenInfra` — the DB pool, NATS, k8s, adapters, bindings) and the generated `internal/app/compose.go` (`NewComponents(infra *Infra) (*Components, error)` — constructs every handler/worker/operator inline). Collaborators are interface-typed fields resolved by type off `infra.<Field>`, so swapping a real in-process service for a Connect client (when you split it into its own Deployment) is a one-line change there, with consumers untouched.

### Phase 4: Evolve the DB schema (migrations lead, projections follow)

```bash
# Create a new migration
forge db migration new add_login_tracking

# Write the SQL
# db/migrations/00002_add_login_tracking.up.sql  (ALTER TABLE users ADD COLUMN ...)
# db/migrations/00002_add_login_tracking.down.sql

# Re-project the entity struct/ORM from the new schema
forge generate

# Apply against a live database
forge db migrate up --dsn "$DATABASE_URL"
```

Write plain postgres DDL — forge is postgres-pinned, so anything postgres accepts works (`::type` casts, schema-qualified names, `JSONB`, `TEXT[]`). See the `db` sub-skill.

### Phase 5: Wire and schema evolve independently

The service-proto messages are the API truth; the schema is the storage truth. The generated conversions map the intersection of wire fields and columns by name — a DB-only column (audit trail, denormalized cache) never leaks onto the wire, and a wire-only field never reaches the DB. Add either side freely; this divergence is the design, not a problem to avoid.

## Adding Components

```bash
forge add service <name>              # New Connect RPC service
forge add service <name> --port 8082  # Specify port
forge add entity <name> [f:type ...]  # DB entity: create-table migration + CRUD proto scaffold
forge add worker <name>               # Background worker (Start/Stop lifecycle)
forge add binary <name>               # Standalone long-running binary (own Deployment)
forge add adapter <name>              # Outbound boundary translator (HTTP client, SDK wrapper)
forge add frontend <name>             # Next.js frontend
forge add webhook <name> --service S  # Webhook endpoint on an existing service
forge add package <name>              # Internal Go package with interface contract
```

All `forge add` commands update `forge.yaml` and run the generation pipeline automatically.

## Key Commands

| Command | What it does |
|---------|-------------|
| `forge generate` | Regenerates infrastructure from protos + applied migrations (safe to re-run anytime) |
| `forge up --env=dev` | Full stack: Docker infra + Go services (hot reload) + frontends. Defaults `ENVIRONMENT=development` and dev CORS origins when unset, and dev mode attaches a synthetic dev user (`devClaims()` in `internal/middleware/middleware.go`) — so the API and generated CRUD are callable with zero auth config |
| `forge up --env=<env>` | Build + deploy + host launch + frontend dev — one command, reads `deploy/kcl/<env>/` |
| `forge test` | Unit + integration tests |
| `forge test e2e` | E2E tests (requires stack running via `forge up --env=dev`) |
| `forge lint` | Go + proto + frontend linters |
| `forge build` | Binaries + frontends + Docker images |
| `forge deploy dev` | Deploy to local k3d cluster (or whatever deploy target dev's KCL declares) |
| `forge db migration new <name>` | Create a new migration pair |
| `forge db migrate up --dsn $DSN` | Apply pending migrations |

## Project shape via `--kind`

```bash
forge new my-app --mod github.com/acme/my-app                   # service (default)
forge new my-app --mod github.com/acme/my-app --kind cli        # CLI binary, no server
forge new my-app --mod github.com/acme/my-app --kind library    # pure Go library, no cmd/
```

Each kind has its own default `features:` block in forge.yaml:

- `service` — every feature enabled (today's behavior).
- `cli` — build/ci/docs enabled; deploy/frontend/packs/observability/codegen disabled.
- `library` — docs/contracts enabled; everything else disabled.

Override per-project in forge.yaml's `features:` block — set
`features.deploy: true` on a CLI to opt back into deploy codegen, etc.
Disabled commands return a clear error explaining how to enable them.

## Port Assignment

Ports are auto-assigned and tracked in `forge.yaml`:
- **Services** auto-increment from `8080` (8080, 8081, 8082, ...)
- **Frontends** auto-increment from `3000` (3000, 3001, 3002, ...)

Override with `--port` on `forge add service` or `forge add frontend`.

Each service and frontend is reachable at its own declared port — read
the port assignments in `forge.yaml` (or the launch banner) to find
where each one binds. If a postgres already runs on the host's 5432,
`forge up --env=dev` fails fast before `docker compose up` and prints
the exact `POSTGRES_PORT=<free> forge up --env=dev` rerun command.

## Rules

- Always run `forge generate` after any proto or migration change.
- Never hand-edit `gen/` or any `*_gen.go` file — they are regenerated. The owned seam `internal/app/providers.go` (`Infra`/`OpenInfra`) is **owned code you wire** (not generated, not off-limits); `internal/app/compose.go` (`NewComponents`) is forge-owned and regenerated every run — `forge disown internal/app/compose.go` to hand-own it when you need construct-then-inject.
- Use `forge add` to scaffold — never copy-paste existing directories.
- Use `forge test`, not raw `go test` — the CLI sets the right build tags.
- One service per proto package. A service's owned and generated files co-locate in one `internal/handlers/<svc>/` directory.
- DB schema changes go through migrations, not proto edits — the SQL in `db/migrations/` is the schema truth; the ORM follows it.
- `forge generate` is always safe — it only touches infrastructure (including the generated `internal/db/<entity>_orm.go`), never your business logic or migrations.
