---
name: getting-started
description: Create a new Forge project, add components, and start the dev loop.
---

# Getting Started with Forge

## Creating a New Project

```bash
forge new <project-name> --mod <go-module-path>
```

### Required flags

| Flag | Description |
|------|-------------|
| `--mod` | Go module path (e.g., `github.com/example/my-project`) — **required** |

### Optional flags

| Flag | Default | Description |
|------|---------|-------------|
| `--service <name>` | _(none)_ | Initial Go service(s) — repeatable or comma-separated |
| `--frontend <name>` | _(none)_ | Initial Next.js frontend(s) — repeatable or comma-separated |
| `--path <dir>` | `.` | Parent directory for the project |
| `--in-place` | `false` | Scaffold into the current directory instead of creating a subdirectory |
| `--go-version` | _(detected)_ | Go version for go.mod (e.g., `1.24`) |
| `--license` | `MIT` | License to include (`MIT`, `Apache-2.0`, `BSD-3-Clause`, `none`) |
| `--license-author` | _(git user.name)_ | Copyright holder for LICENSE |
| `--force` | `false` | Overwrite existing project config |

### Examples

```bash
# Minimal — creates project dir, no initial service
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
├── pkg/app/                   # bootstrap.go (generated) + setup.go (yours)
├── pkg/middleware/             # Auth, logging, tenant middleware
├── pkg/config/                # Config struct + loader
├── db/migrations/             # SQL migration directory
├── db/queries/                # sqlc query directory
├── deploy/                    # Docker, KCL, observability configs
├── e2e/                       # E2E test directory
├── forge.yaml                 # Project config
├── docker-compose.yml         # Dev infra (Postgres, LGTM, etc.)
├── Taskfile.yml               # Task runner aliases
└── .reliant/skills/forge/     # These skills
```

## First Steps After Creation

```bash
cd my-app

# If you didn't include --service in forge new:
forge add service <name>

# Define RPCs in proto/services/<svc>/v1/<svc>.proto
# Then generate code:
forge generate

# Implement handlers in handlers/<svc>/handlers.go
```

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

## The Dev Loop

```
edit proto  →  forge generate  →  implement handlers  →  forge test  →  forge run
```

| Command | What it does |
|---------|-------------|
| `forge generate` | Rebuilds all generated code from proto definitions |
| `forge run` | Full stack: Docker infra + Go services (hot reload) + frontends |
| `forge test` | Unit + integration tests |
| `forge test e2e` | E2E tests (requires stack running via `forge run`) |
| `forge lint` | Go + proto + frontend linters |
| `forge build` | Binaries + frontends + Docker images |
| `forge deploy dev` | Deploy to local k3d cluster |

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
