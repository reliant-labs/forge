---
title: "CLI Reference"
description: "Complete reference for all Forge CLI commands, flags, and examples"
weight: 10
---

# CLI Reference

## Installation

```bash
go install github.com/reliant-labs/forge/cmd/forge@latest
```

## Global Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--config` | Path to config file | `./forge.project.yaml` |
| `--verbose`, `-v` | Verbose output | `false` |
| `--version` | Print version information | — |
| `--help`, `-h` | Show help | — |

---

## `forge new`

Create a new Forge project.

```bash
forge new <project-name> --mod <module-path> [flags]
```

**Flags:**

| Flag | Description | Default |
|------|-------------|---------|
| `--mod` | Go module path (required) | — |
| `--service` | Name of the initial Go service | `api` |
| `--frontend` | Name of an initial Next.js frontend (optional) | — |
| `-p`, `--path` | Parent directory for the project | `.` |

**What it creates:**

- Single binary entrypoint: `cmd/main.go`, `cmd/server.go`, `cmd/version.go`
- Wiring code: `pkg/app/wire.go`, `pkg/app/wire_test.go`, `pkg/app/testing.go`
- Initial service under `services/<service-name>/`
- Proto stubs in `proto/config/v1/` and `proto/services/<service-name>/v1/`
- Single Dockerfile at `deploy/Dockerfile`
- KCL manifests, CI/CD workflows, supporting config files
- Git repository with initial commit

**Examples:**

```bash
forge new myproject --mod github.com/myorg/myproject
forge new myproject --mod github.com/myorg/myproject --frontend dashboard
```

---

## `forge add`

### `forge add service`

```bash
forge add service <name> [flags]
```

**Flags:**

| Flag | Description | Default |
|------|-------------|---------|
| `--port` | Service port | Auto-increment from 8080 |

**What it creates:**

- `services/<name>/service.go` — scaffold with `New(deps)`, `Register(mux, opts)`, `Name()`
- `services/<name>/handlers.go` — placeholder for business logic
- `proto/services/<name>/v1/<name>.proto` — empty proto service stub
- Updates `pkg/app/wire.go` with construction and registration logic
- Updates `forge.project.yaml`

**Examples:**

```bash
forge add service users
forge add service orders --port 8082
```

### `forge add frontend`

```bash
forge add frontend <name> [flags]
```

**Flags:**

| Flag | Description | Default |
|------|-------------|---------|
| `--port` | Dev server port | Auto-increment from 3000 |

**What it creates:**

- `frontends/<name>/` — Next.js application scaffold
- Updates `forge.project.yaml`

---

## `forge package new`

Create a new internal package with a Go interface contract.

```bash
forge package new <name>
```

**What it creates:**

- `internal/<name>/contract.go` — Go interface (the contract)
- `internal/<name>/service.go` — implementation scaffold
- `internal/<name>/mock_gen.go` — generated mock
- `internal/<name>/middleware_gen.go` — generated logging/tracing wrapper
- Updates `pkg/app/wire.go`

**Examples:**

```bash
forge package new email
forge package new notifications
```

---

## `forge generate`

Generate code from proto files and contract interfaces.

```bash
forge generate [flags]
```

**Flags:**

| Flag | Description | Default |
|------|-------------|---------|
| `-w`, `--watch` | Watch for changes and regenerate | `false` |
| `-f`, `--force` | Force regeneration of config files | `false` |

**Pipeline:**

1. `buf generate` — Go protobuf + Connect RPC stubs
2. `buf generate` — TypeScript stubs for frontends
3. Service stubs (non-destructive)
4. Mock/middleware generation
5. Wiring code update (`pkg/app/wire.go`)
6. `sqlc generate` (if `sqlc.yaml` exists)
7. `go mod tidy` in `gen/`

---

## `forge build`

```bash
forge build [flags]
```

**Flags:**

| Flag | Description | Default |
|------|-------------|---------|
| `-o`, `--output` | Output directory for binary | `bin` |
| `-t`, `--target` | Build target | `all` |
| `--parallel` | Build in parallel | `true` |
| `--docker` | Also build Docker image | `false` |

---

## `forge run`

```bash
forge run [flags]
```

**Flags:**

| Flag | Description | Default |
|------|-------------|---------|
| `--env` | Environment to run | `dev` |
| `--no-infra` | Skip docker-compose | `false` |

Starts infrastructure (docker-compose), Go binary (via Air for hot reload), and Next.js frontends.

---

## `forge deploy`

```bash
forge deploy <environment> [flags]
```

**Flags:**

| Flag | Description | Default |
|------|-------------|---------|
| `--image-tag` | Image tag | Git short SHA |
| `--dry-run` | Print manifests without applying | `false` |
| `--namespace` | Override namespace | `<project>-<env>` |

---

## `forge test`

```bash
forge test [subcommand] [flags]
```

**Subcommands:** `unit`, `integration`, `e2e`

**Flags:**

| Flag | Description | Default |
|------|-------------|---------|
| `--race` | Enable race detector | `true` |
| `--coverage` | Generate coverage reports | `false` |
| `--parallel` | Run in parallel | `true` |
| `-V`, `--test-verbose` | Verbose test output | `false` |

---

## `forge lint`

```bash
forge lint [paths...] [flags]
```

**Flags:**

| Flag | Description | Default |
|------|-------------|---------|
| `--contract` | Run contract enforcement linter | `false` |
| `--fix` | Auto-fix issues | `false` |

**Standard linting:** golangci-lint, buf lint, TypeScript linters.

**Contract enforcement (`--contract`):** Verifies all exported methods match their contract (proto RPC or Go interface).

---

## `forge db`

### `forge db migration new`

```bash
forge db migration new <name> [--dir db/migrations]
```

### `forge db migrate up|down|status|version|force`

```bash
forge db migrate up --dsn <connection-string> [--dir db/migrations]
forge db migrate down --dsn <connection-string>
forge db migrate status --dsn <connection-string>
forge db migrate version --dsn <connection-string>
forge db migrate force <version> --dsn <connection-string>
```

Requires the `migrate` CLI from [golang-migrate](https://github.com/golang-migrate/migrate).

---

## Generated Binary Commands

Each generated project produces a single binary with Cobra commands:

### `<binary> server [services...]`

Start the HTTP server. Optionally specify which services to register.

```bash
myproject server                    # All services
myproject server api users          # Only named services
```

### `<binary> version`

Print build information.