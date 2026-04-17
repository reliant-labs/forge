# CLI Reference

Complete reference for every `forge` command, subcommand, and flag.

## Global Flags

These flags are available on all commands:

| Flag | Default | Description |
|------|---------|-------------|
| `--config <path>` | `./forge.yaml` | Path to the Forge config file |
| `--verbose`, `-v` | `false` | Enable verbose output |
| `--version` | | Print version information and exit |

## forge new

Create a new Forge project.

```
forge new <project-name> --mod <module-path> [flags]
```

**Arguments:**

| Argument | Required | Description |
|----------|----------|-------------|
| `project-name` | Yes | Name of the project directory to create |

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--mod <module>` | (required) | Go module path (e.g., `github.com/example/myproject`) |
| `--service <name>` | `api` | Name of the initial Go service |
| `--frontend <name>` | (none) | Name of an initial Next.js frontend (optional) |
| `--path`, `-p` | `.` | Parent directory where the project is created |

**What it generates:**

- Project directory with `forge.yaml`
- Single binary entrypoint: `cmd/main.go`, `cmd/server.go`, `cmd/version.go`
- Proto directory structure (`proto/config/`, `proto/services/`, `proto/forge/`)
- Initial service with `service.go` and `handlers.go` under `services/<name>/`
- Wiring code: `pkg/app/wire.go`, `pkg/app/wire_test.go`
- Single Dockerfile at `deploy/Dockerfile`
- KCL deploy configs for dev, staging, and prod
- CI/CD workflows (`.github/workflows/`)
- Supporting files: `buf.yaml`, `buf.gen.yaml`, `Taskfile.yml`, `.gitignore`, `.golangci.yml`, `docker-compose.yml`
- Git repository with initial commit

**Examples:**

```bash
forge new myproject --mod github.com/example/myproject
forge new myproject --mod github.com/example/myproject --service gateway --frontend web
```

---

## forge add service

Add a new Go service to an existing project.

```
forge add service <name> [flags]
```

Must be run from the project root (where `forge.yaml` exists).

**Arguments:**

| Argument | Required | Description |
|----------|----------|-------------|
| `name` | Yes | Service name (used for directory, proto package, and handler registration) |

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--port <int>` | auto-increment from 8080 | Service port number |

**What it creates:**

- `services/<name>/service.go` -- service scaffold with `New(deps)`, `Register(mux, opts)`, `Name()`
- `services/<name>/handlers.go` -- placeholder for business logic
- `proto/services/<name>/v1/<name>.proto` -- empty proto service stub
- Updates `pkg/app/wire.go` with construction and registration logic
- Updates `forge.yaml`

**Examples:**

```bash
forge add service users
forge add service orders --port 8082
```

---

## forge add frontend

Add a new Next.js frontend to an existing project.

```
forge add frontend <name> [flags]
```

**Arguments:**

| Argument | Required | Description |
|----------|----------|-------------|
| `name` | Yes | Frontend name (used for directory and project config) |

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--port <int>` | `3000` | Dev server port |

**What it creates:**

- `frontends/<name>/` -- Next.js application scaffold
- Connect RPC client configuration pointed at the first service's port
- Updates `forge.yaml`

**Examples:**

```bash
forge add frontend web
forge add frontend dashboard --port 3001
```

---

## forge package new

Create a new internal package with a Go interface contract.

```
forge package new <name>
```

**Arguments:**

| Argument | Required | Description |
|----------|----------|-------------|
| `name` | Yes | Package name (used for directory under `internal/`) |

**What it creates:**

- `internal/<name>/contract.go` -- Go interface (the contract)
- `internal/<name>/service.go` -- implementation scaffold with `New(deps)` constructor
- `internal/<name>/mock_gen.go` -- generated mock implementation
- `internal/<name>/middleware_gen.go` -- generated logging/tracing wrapper
- Updates `pkg/app/wire.go` with construction logic

**Examples:**

```bash
forge package new email
forge package new notifications
```

---

## forge generate

Generate code from proto files and contract interfaces.

```
forge generate [flags]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--watch`, `-w` | `false` | Watch for proto file changes and regenerate automatically |

**Generation pipeline (config-based, when `forge.yaml` exists):**

1. `buf generate` for Go protobuf + Connect RPC stubs
2. `buf generate` for TypeScript stubs for each Next.js frontend
3. Service stub generation for new services (won't overwrite existing files)
4. Mock generation for API services and internal packages
5. Middleware wrapper generation for internal packages
6. Wiring code update (`pkg/app/wire.go`)
7. `sqlc generate` if `sqlc.yaml` or `sqlc.yml` exists
8. `go mod tidy` in `gen/`

**Examples:**

```bash
forge generate
forge generate --watch
```

---

## forge build

Build the service binary and Docker image.

```
forge build [flags]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--output`, `-o` | `bin` | Output directory for the binary |
| `--target`, `-t` | `all` | Build target: `all`, or a specific service/frontend name |
| `--parallel` | `true` | Build in parallel |
| `--docker` | `false` | Also build the Docker image |

The Go binary is built with `CGO_ENABLED=0` and `-ldflags="-s -w"` for a minimal static binary.

**Examples:**

```bash
forge build
forge build --docker
forge build -o dist
```

---

## forge run

Start the development environment with hot reload.

```
forge run [flags]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--env` | `dev` | Environment to run (`dev`, `staging`, `prod`) |
| `--no-infra` | `false` | Skip docker-compose infrastructure |

Starts three layers:

1. **Infrastructure** -- `docker compose up -d` using `deploy/docker-compose.yml`
2. **Go binary** -- via [Air](https://github.com/air-verse/air) for hot reload
3. **Next.js frontends** -- each frontend via `npm run dev`

All output is color-coded by process name. Ctrl+C gracefully stops all processes and tears down docker-compose.

**Examples:**

```bash
forge run
forge run --env=staging
forge run --no-infra
```

---

## forge deploy

Deploy to a Kubernetes environment using KCL manifests.

```
forge deploy <environment> [flags]
```

**Arguments:**

| Argument | Required | Description |
|----------|----------|-------------|
| `environment` | Yes | Target environment (must match a directory under `deploy/kcl/`) |

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--image-tag` | git short SHA | Image tag for deployed containers |
| `--dry-run` | `false` | Print generated manifests without applying |
| `--namespace` | `<project>-<env>` | Override the Kubernetes namespace |

**Behavior for `dev` environment:**

1. Ensures a k3d cluster exists (creates one with a local registry at `localhost:5050` if not)
2. Builds Docker image and pushes to the local registry
3. Generates manifests and applies them

**Examples:**

```bash
forge deploy dev
forge deploy staging --image-tag sha-abc1234
forge deploy prod --image-tag v1.2.0 --dry-run
```

---

## forge test

Run tests across Go services and frontend apps.

```
forge test [subcommand] [flags]
```

**Subcommands:**

| Subcommand | Description |
|------------|-------------|
| (none) | Run all tests (Go + frontend) |
| `unit` | Run unit tests only |
| `integration` | Run integration tests only (includes `integration` build tag) |
| `e2e` | Run end-to-end tests |

**Flags (available on all subcommands):**

| Flag | Default | Description |
|------|---------|-------------|
| `--race` | `true` | Enable Go race detector |
| `--coverage` | `false` | Enable coverage reporting (outputs `coverage.out`) |
| `--parallel` | `true` | Run test suites in parallel |
| `--test-verbose`, `-V` | `false` | Verbose test output |

**Examples:**

```bash
forge test
forge test unit
forge test integration
forge test e2e
forge test --coverage
```

---

## forge lint

Run linters on the project.

```
forge lint [paths...] [flags]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--contract` | `false` | Run contract enforcement linter |
| `--fix` | `false` | Automatically fix issues where possible (golangci-lint only) |

**Standard linting (default):** Runs golangci-lint, buf lint, and TypeScript linters (npm run lint + typecheck) for any Next.js frontends.

**Contract enforcement (`--contract`):** Scans service and package types, reporting exported methods that don't match their contract interface (proto RPC for API services, Go interface for internal packages). Exit code 3 indicates violations found.

**Examples:**

```bash
forge lint
forge lint --contract
forge lint --contract ./services/users
forge lint --fix
```

---

## forge db

Database migration management.

### forge db migration new

Create a new blank SQL migration pair.

```
forge db migration new <name> [flags]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--dir` | `db/migrations` | Migrations directory |

**Examples:**

```bash
forge db migration new add_users_table
forge db migration new "backfill account status"
```

### forge db migrate up

Apply pending migrations. Requires the `migrate` CLI from [golang-migrate](https://github.com/golang-migrate/migrate/tree/master/cmd/migrate).

```
forge db migrate up --dsn <connection-string> [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--dsn` | (required) | Database connection string |
| `--dir` | `db/migrations` | Migrations directory |

### forge db migrate down

Roll back the most recent migration.

```
forge db migrate down --dsn <connection-string> [flags]
```

### forge db migrate status

Show the current migration status.

```
forge db migrate status --dsn <connection-string> [flags]
```

### forge db migrate version

Show the current migration version.

```
forge db migrate version --dsn <connection-string> [flags]
```

### forge db migrate force

Force the recorded migration version without running SQL.

```
forge db migrate force <version> --dsn <connection-string> [flags]
```

**Migration examples:**

```bash
forge db migration new "add users table"
forge db migrate up --dsn "postgres://localhost:5432/myproject?sslmode=disable"
forge db migrate down --dsn "postgres://localhost:5432/myproject?sslmode=disable"
forge db migrate status --dsn "postgres://localhost:5432/myproject?sslmode=disable"
```

---

## Generated Binary Commands

Each generated project produces a single binary with these Cobra commands:

### `<binary> server [services...]`

Start the HTTP server with Connect RPC handlers.

```bash
myproject server                    # Start all registered services
myproject server api users          # Start only named services
```

The server registers each service's Connect handlers onto a shared HTTP mux, applies middleware interceptors, and starts listening.

### `<binary> version`

Print build information.

```bash
myproject version
```

---

## Project Configuration

All commands that operate on a project read `forge.yaml` from the current directory.

```yaml
name: myproject
module_path: github.com/myorg/myproject
version: "0.1.0"
hot_reload: true

services:
  - name: api
    type: GO_SERVICE
    path: services/api
    port: 8080
  - name: users
    type: GO_SERVICE
    path: services/users
    port: 8081

frontends:
  - name: dashboard
    type: nextjs
    path: frontends/dashboard
    port: 3000

environments:
  - name: dev
    type: local
    registry: localhost:5050
    namespace: myproject-dev
  - name: staging
    type: cloud
    registry: ghcr.io/myorg
    namespace: myproject-staging
  - name: prod
    type: cloud
    registry: ghcr.io/myorg
    namespace: myproject-prod

database:
  driver: postgres
  migrations_dir: db/migrations
  sqlc_enabled: true

docker:
  registry: ghcr.io/myorg

k8s:
  provider: k3d
  kcl_dir: deploy/kcl

ci:
  provider: github
  lint: true
  test: true
  build: true
  deploy: true
  vuln_scan: true
```