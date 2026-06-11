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
`pkg/app` wiring, buf/proto scaffolding, Taskfile/CI/deploy — and
`services: []` in forge.yaml. The binary is a deployment unit that
mounts services; it is **not** a domain entity, so forge never invents
a `<project>Service` from the binary name. The first step after a bare
scaffold is:

```bash
forge add service <entity>   # name it after a domain entity (item, order, user), not the binary
```

Pass `--service <entity>` at creation time to opt into an initial
service (it ships an example `Item` CRUD proto following the forge
naming convention).

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

```
my-app/
├── proto/services/<svc>/v1/   # Proto definitions (if --service given)
├── handlers/<svc>/            # Go handler skeleton (if --service given)
├── frontends/<name>/          # Next.js app (if --frontend given)
├── internal/db/               # DB types (aliases) + ORM functions
├── pkg/app/                   # bootstrap.go (generated) + setup.go (yours)
├── pkg/middleware/             # Auth, logging, tenant middleware
├── pkg/config/                # Config struct + loader
├── db/migrations/             # SQL migration directory (initial schema)
├── db/queries/                # SQL query directory
├── deploy/                    # Docker, KCL, observability configs
├── e2e/                       # E2E test directory
├── forge.yaml                 # Project config
├── docker-compose.yml         # Dev infra (Postgres, LGTM, etc.)
├── Taskfile.yml               # Task runner aliases
└── .reliant/skills/forge/     # These skills
```

### Ingress scaffolding

For `--kind service` projects (the default), forge scaffolds Gateway
API ingress alongside the deploy config:

- `deploy/kcl/ingress.k` — project-wide gateway topology (one `public`
  Gateway with `HTTP` + `H2C` listeners by default). User-owned.
- `deploy/kcl/<env>/ingress.k` — per-env override (dev re-exports the
  base; staging/prod show commented `tls` blocks ready to fill in).
- An example commented `HTTPRoute` in the base file showing how to
  attach the initial service.

Each env's `main.k` wires `ing.GATEWAYS` / `ing.HTTP_ROUTES` /
`ing.GRPC_ROUTES` into the Bundle. See the `ingress` sub-skill for the
full story (schemas, TLS, providers, the "no port-forward" contract).

## The Development Workflow

Proto gets you a working demo fast. After that, your DB schema and entity types evolve independently.

### Phase 1: Scaffold (proto gets you started)

```bash
forge new my-app --mod github.com/acme/my-app --service users --frontend web
cd my-app
```

Proto defines your initial API surface. Forge scaffolds everything — handlers, DB types as proto aliases, an initial migration, middleware, mocks, and frontend hooks.

### Phase 2: Define your API surface

Edit proto to define RPCs and request/response messages:

```
proto/services/users/v1/users.proto
```

Then generate infrastructure from it:

```bash
forge generate
```

This rebuilds `gen/` (Go stubs, TS clients, mocks, middleware, wiring). It does **not** touch your handlers, DB layer, or business logic.

### Phase 3: Implement business logic

Write your handler logic in `handlers/<svc>/service.go`. Use the generated types and your `internal/db/` ORM functions.

### Phase 4: Evolve the DB schema (independently of proto)

As your domain grows, the database schema will diverge from your API shape. This is expected and correct.

```bash
# Create a new migration
forge db migration new add_login_tracking

# Write the SQL
# db/migrations/00002_add_login_tracking.up.sql
# db/migrations/00002_add_login_tracking.down.sql

# Apply it
forge db migrate up --dsn "$DATABASE_URL"
```

Update entity types and ORM functions to match — see the `db` sub-skill.

### Phase 5: Entity types diverge from proto

Entity types start as proto aliases (`type User = apiv1.User`). When your DB has fields the API doesn't (or vice versa), replace the alias with a concrete Go struct and add mapper functions. This is the natural evolution — not a problem to avoid.

## Adding Components

```bash
forge add service <name>              # New Connect RPC service
forge add service <name> --port 8082  # Specify port
forge add worker <name>               # Background worker (Start/Stop lifecycle)
forge add frontend <name>             # Next.js frontend
forge add webhook <name> --service S  # Webhook endpoint on an existing service
forge add package <name>              # Internal Go package with interface contract
```

All `forge add` commands update `forge.yaml` and run the generation pipeline automatically.

## Key Commands

| Command | What it does |
|---------|-------------|
| `forge generate` | Regenerates infrastructure from proto (safe to re-run anytime) |
| `forge run` | Full stack: Docker infra + Go services (hot reload) + frontends |
| `forge test` | Unit + integration tests |
| `forge test e2e` | E2E tests (requires stack running via `forge run`) |
| `forge lint` | Go + proto + frontend linters |
| `forge build` | Binaries + frontends + Docker images |
| `forge deploy dev` | Deploy to local k3d cluster |
| `forge db migration new <name>` | Create a new migration pair |
| `forge db migrate up --dsn $DSN` | Apply pending migrations |

## Port Assignment

Ports are auto-assigned and tracked in `forge.yaml`:
- **Services** auto-increment from `8080` (8080, 8081, 8082, ...)
- **Frontends** auto-increment from `3000` (3000, 3001, 3002, ...)

Override with `--port` on `forge add service` or `forge add frontend`.

## Rules

- Always run `forge generate` after any proto change.
- Never hand-edit `gen/` or `bootstrap.go` — they are regenerated.
- Use `forge add` to scaffold — never copy-paste existing directories.
- Use `forge test`, not raw `go test` — the CLI sets the right build tags.
- One service per proto package. One handler directory per service.
- DB schema changes go through migrations, not proto edits.
- `forge generate` is always safe — it only touches infrastructure, never your business logic or DB layer.
