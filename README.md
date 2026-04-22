# Forge

Proto-first application framework for Go + Next.js monorepos. Define your services in protobuf, and Forge generates handlers, database operations, TypeScript clients, Kubernetes deployments, and CI/CD pipelines — with an architecture optimized for LLM-assisted development.

## Why Forge

LLMs are powerful code generators, but they struggle with ambiguous architectures, sprawling codebases, and no way to confirm their output actually works. Forge solves this by generating projects that are purpose-built for AI-assisted development:

**Reduced context, better generation.** Forge projects are contract-driven. Proto files and Go interfaces define every boundary, so an LLM reads the contract, generates the implementation, and moves on. Less context consumed means higher quality output.

**Clean architecture, predictable patterns.** Every service follows the same structure: constructor injection via `Deps` structs, explicit dependency wiring, no global state, no `init()` side effects. An LLM that has seen one Forge service can generate the next one correctly.

**Built-in feedback loops.** Forge generates test harnesses, mock dependencies, contract linters, and a full dev environment out of the box. An LLM can generate a handler, run `forge test`, see the failure, fix it, and iterate — all without human intervention.

## Feature Highlights

**CRUD Codegen from Proto Entities.** Define an entity in protobuf and Forge generates complete Create/Get/List/Update/Delete handlers with tests. List operations include cursor-based keyset pagination, search and filtering (ILIKE, exact match, ordering), and soft delete support — all driven by proto annotations.

**Multi-Tenant Isolation.** Add a `tenant_key` annotation to an entity and Forge generates row-level tenant filtering across all ORM operations, plus middleware that extracts the tenant ID from JWT claims. No manual plumbing required.

**RBAC Policy Codegen.** Annotate RPC methods with `required_roles` and Forge generates an authorizer that maps procedures to required roles, wired into the middleware stack.

**TypeScript Hooks.** For every RPC, Forge auto-generates TanStack React Query hooks — queries for read operations, mutations for writes — ready to use in your Next.js frontend.

**Pack System.** Install pre-built feature packs (`jwt-auth`, `clerk`, `stripe`, `twilio`, `api-key`, `audit-log`) that add real, working code — not just boilerplate stubs.

**Full Observability Stack.** Every project ships with Grafana LGTM (Prometheus, Tempo, Loki, Pyroscope), Grafana Alloy collector, pre-built dashboards, structured logging, and OpenTelemetry tracing. Run `forge doctor` to validate everything is healthy.

**Delve Debugging.** Debug running services with `forge debug start`, set breakpoints, evaluate expressions, and inspect goroutines — all from the CLI.

## Quick Start

```bash
# Install
go install github.com/reliant-labs/forge/cmd/forge@latest

# Create a project with a service and frontend
forge new myproject --mod github.com/example/myproject --service api --frontend web

# Enter the project
cd myproject

# Define your RPCs in proto/services/api/v1/api.proto, then generate everything
forge generate

# Start the dev environment (Postgres, observability stack, hot-reload)
forge run

# Deploy to a local k3d cluster
forge deploy dev
```

After `forge new`, your project looks like this:

```
myproject/
├── forge.yaml                       # Project configuration
├── cmd/                             # Single binary entrypoint (Cobra CLI)
│   ├── main.go
│   ├── server.go                    # server [services...] — HTTP server
│   └── version.go
├── proto/
│   ├── services/api/v1/api.proto    # Service definitions (you edit these)
│   ├── db/v1/                       # Entity definitions for CRUD codegen
│   ├── config/v1/                   # Config proto — instantiation contract
│   └── forge/options/v1/            # Forge proto annotations
├── gen/                             # Generated Go + Connect stubs
├── handlers/
│   └── api/
│       ├── service.go               # New(deps), Register(mux), Name()
│       ├── handlers.go              # Business logic (you write this)
│       ├── handlers_crud_gen.go     # Generated CRUD handlers
│       ├── handlers_crud_test_gen.go
│       └── authorizer_gen.go        # Generated RBAC policy
├── internal/                        # Internal packages (Go interface contracts)
│   └── <name>/
│       ├── contract.go              # Go interface — THE contract
│       ├── service.go               # Implementation
│       ├── mock_gen.go              # Generated mock
│       └── middleware_gen.go        # Generated logging/tracing wrapper
├── pkg/
│   ├── app/
│   │   ├── bootstrap.go             # Generated — constructs all services
│   │   └── testing.go               # Generated — test helpers with mocks
│   └── middleware/                   # Auth, tenant, rate limit, idempotency, etc.
├── frontends/
│   └── web/
│       └── src/gen/                 # Generated TypeScript clients + React Query hooks
├── db/
│   └── migrations/                  # SQL migration files (source of truth)
├── deploy/
│   ├── kcl/                         # KCL Kubernetes manifests (dev/staging/prod)
│   ├── Dockerfile                   # Multi-stage build
│   ├── k3d.yaml                     # Local cluster config
│   └── docker-compose.yml           # Postgres + observability stack
├── .github/workflows/               # CI/CD (lint → test → build → deploy)
├── Taskfile.yml
├── buf.yaml / buf.gen.yaml
└── go.work / go.mod
```

## CLI Reference

### Project Lifecycle

| Command | Description |
|---------|-------------|
| `forge new <name> --mod <module>` | Create a new project. Optional: `--service <name>`, `--frontend <name>`, `--license <type>`. |
| `forge generate` | Generate all code from protos and contracts. Use `--watch` for dev mode, `--force` to regenerate config files. |
| `forge build` | Build the Go binary and frontends. Supports `--docker`, `--target <name>`, `--debug` (Delve symbols). |
| `forge run` | Start the dev environment: Docker Compose infra, Go services via Air (hot reload), Next.js frontends. |
| `forge deploy <env>` | Deploy to Kubernetes via KCL. For `dev`, auto-creates a k3d cluster. Supports `--dry-run`, `--image-tag`. |
| `forge upgrade` | Update frozen project files from latest Forge templates. Use `--check` for dry-run, `--force` to overwrite. |

### Adding Components

| Command | Description |
|---------|-------------|
| `forge add service <name>` | Add a Go service with proto file, handler scaffold, and wiring. Optional: `--port`. |
| `forge add worker <name>` | Add a background worker. |
| `forge add operator <name>` | Add a Kubernetes operator. Optional: `--group`, `--version`. |
| `forge add frontend <name>` | Add a Next.js frontend with Connect RPC client setup and TypeScript codegen. |
| `forge add webhook <name>` | Add a webhook endpoint to a service. Requires `--service`. |
| `forge add package <name>` | Add an internal package with contract interface, implementation, mock, and middleware wrapper. |

### Testing and Quality

| Command | Description |
|---------|-------------|
| `forge test` | Run all tests (unit + integration). Subcommands: `unit`, `integration`, `e2e`. |
| `forge lint` | Run golangci-lint, buf lint, TypeScript linters, and contract enforcement. Use `--contract` or `--db` for specific checks. |
| `forge doctor` | Diagnose the local dev stack: Docker, app health, Prometheus, Tempo, Loki, Pyroscope, Delve. |

### Database

| Command | Description |
|---------|-------------|
| `forge db migration new <name>` | Create a new SQL migration pair (up/down). Use `--from-proto` to generate from entity definitions. |
| `forge db migrate <up\|down\|status\|version\|force>` | Run or inspect migrations via golang-migrate. |
| `forge db introspect` | Introspect the live database schema. |
| `forge db proto` | Sync proto entity definitions from the migrated schema. |
| `forge db codegen` | Generate ORM code from entity definitions. |

### Packs and Utilities

| Command | Description |
|---------|-------------|
| `forge pack list` | List available feature packs. |
| `forge pack install <name>` | Install a pack (renders templates, adds deps, updates forge.yaml). |
| `forge pack remove <name>` | Remove a pack's files from the project. |
| `forge debug <subcommand>` | Debug a running service with Delve (`start`, `break`, `continue`, `eval`, `step`, `locals`, `stack`, `stop`). |
| `forge docs generate` | Generate documentation from proto definitions and contracts. Supports `--format=hugo`. |
| `forge scaffold-from-plan <file>` | Scaffold a complete project from a YAML plan file (services, packages, frontends in one batch). |
| `forge package new <name>` | Create an internal package with contract, implementation, mock, and middleware. |

## Code Generation

Forge's code generation pipeline runs on `forge generate` and produces code from two sources: proto definitions and Go interface contracts.

### From Proto Definitions

When you define services and entities in `.proto` files, `forge generate` produces:

- **Go server stubs** via Connect RPC (protoc-gen-go + protoc-gen-connect-go)
- **CRUD handlers and tests** from entity definitions in `proto/db/` — full Create, Get, List, Update, Delete implementations
- **Cursor-based pagination** for all List RPCs following AIP-158 conventions
- **Search and filtering** from request message fields — ILIKE for string fields, exact match for IDs, configurable ordering
- **Soft delete handling** when `soft_delete = true` is set on an entity — `deleted_at` column managed automatically in all ORM operations
- **Tenant-scoped queries** when `tenant_key` is annotated — automatic WHERE clause injection across all generated CRUD
- **RBAC authorizer** from `required_roles` method annotations — maps RPC procedures to allowed roles
- **Auth middleware** from `forge.yaml` auth config — JWT (HS256/RS256/JWKS), API key, or dual auth
- **Tenant middleware** from multi-tenant config — extracts tenant ID from JWT claims
- **TypeScript clients** for Next.js frontends via buf
- **React Query hooks** per service — `useQuery` for reads (Get/List/Search), `useMutation` for writes
- **Seed data** — realistic SQL and JSON fixtures generated from entity definitions
- **Bootstrap wiring** in `pkg/app/bootstrap.go` — explicit service construction with all dependencies

### From Go Interface Contracts

Internal packages define contracts as Go interfaces in `contract.go` files. These are richer than proto — supporting channels, complex types, factories, and variadic arguments. From each contract, `forge generate` produces:

- **Mock implementations** via Go AST analysis
- **Middleware wrappers** for logging and tracing
- **Metrics wrappers** for instrumentation

### Proto Annotations

Forge extends protobuf with custom annotations to drive code generation:

```protobuf
// Service-level: auth defaults, visibility, dependencies
option (forge.options.v1.service_options) = {
  auth: { auth_required: true, auth_provider: "jwt" }
};

// Method-level: per-RPC auth, idempotency, timeouts
option (forge.options.v1.method_options) = {
  auth_required: true,
  idempotent: true,
  idempotency_key: true
};

// Entity-level: table mapping, soft delete, timestamps, indexes
option (forge.options.v1.entity_options) = {
  table_name: "patients",
  soft_delete: true,
  timestamps: true
};

// Field-level: primary key, column type, constraints, validation
option (forge.options.v1.field_options) = {
  primary_key: true,
  not_null: true,
  validation: { required: true, format: "uuid" }
};
```

## Architecture

### Single Binary

Each project produces a single Go binary with a Cobra CLI. All services share one HTTP mux, one middleware stack, and one process:

```bash
myproject server                    # Start all services
myproject server api users          # Start only specific services
myproject version                   # Print build info
```

### Constructor Injection

Services and internal packages receive dependencies through `Deps` structs — no global state, no `init()` side effects:

```go
type Deps struct {
    DB     *pgxpool.Pool
    Logger *slog.Logger
    Users  users.Contract  // Go interface from internal/users/contract.go
}

func New(deps Deps) *Service { return &Service{deps: deps} }
```

Dependencies are wired explicitly in the generated `pkg/app/bootstrap.go`. The test helper `pkg/app/testing.go` provides `NewTestXxx` functions that wire services with mock dependencies.

### Two Contract Systems

**Proto contracts** define external boundaries. Service RPCs in `proto/services/` and config in `proto/config/` drive Go server stubs and TypeScript clients via Connect RPC.

**Go interface contracts** define internal boundaries. Packages in `internal/` use `contract.go` interfaces that support the full richness of Go's type system — channels, complex types, factories, variadic arguments.

### Migration-First Database

SQL migrations in `db/migrations/` are the source of truth for schema. Proto entity definitions in `proto/db/` describe the application-level view and are validated against the migrated schema. The `pkg/orm/` library generates thin CRUD operations over `database/sql` — no heavy ORM. For complex queries, use [sqlc](https://sqlc.dev/) alongside the generated ORM.

### Connect RPC Everywhere

All service communication uses [Connect RPC](https://connectrpc.com/), which is wire-compatible with gRPC but also works over HTTP/1.1 with JSON. Go services talk to each other via Connect, the Next.js frontend calls the same services with full type safety, and you can debug API calls with `curl`. No gRPC-Web proxies required.

## Pack System

Packs are installable feature modules that add real, working code to your project — not just boilerplate stubs. Each pack renders templates with your project's module path, adds Go dependencies, and records itself in `forge.yaml`.

| Pack | What It Adds |
|------|-------------|
| `jwt-auth` | JWT validation middleware (HS256/RS256), JWKS support, dev-mode auth bypass |
| `clerk` | Clerk authentication integration with webhook handler for user sync |
| `stripe` | Stripe client, webhook handler with signature verification, payment entity definitions |
| `twilio` | Twilio client, SMS/voice service with contract interface, webhook handler |
| `api-key` | API key validation middleware, key store with migration, `KeyValidator` interface |
| `audit-log` | Audit log interceptor, persistent store with migration, structured audit events |

```bash
forge pack list                    # See available packs
forge pack install jwt-auth        # Install JWT authentication
forge pack install stripe          # Add Stripe integration
forge pack remove stripe           # Remove a pack
```

## Auth and Multi-Tenancy

Authentication and multi-tenancy are configured in `forge.yaml` and code-generated into your middleware stack.

**JWT Auth** supports HS256 and RS256 signing, JWKS endpoint discovery, configurable claim extraction, and per-method auth overrides via proto annotations. **API Key Auth** generates a validation interceptor and `KeyValidator` interface you implement against your key store. **Dual Auth** supports both JWT and API key on the same service.

**Multi-Tenant Isolation** extracts a tenant ID from JWT claims (configurable field, e.g. `org_id`) and injects it into a request-scoped context. All generated CRUD operations automatically include a tenant WHERE clause. The tenant column name is configurable in `forge.yaml`.

## Observability

Every Forge project includes a complete observability stack configured in Docker Compose:

- **Prometheus** for metrics collection
- **Tempo** for distributed tracing
- **Loki** for log aggregation
- **Pyroscope** for continuous profiling
- **Grafana** with pre-built dashboards (Overview, Logs, Traces)
- **Grafana Alloy** as the unified collector

Services emit structured log events, OpenTelemetry traces, and Prometheus metrics out of the box. Run `forge doctor` to verify that every signal is flowing correctly — it checks Docker containers, app health, pprof endpoints, and each telemetry backend.

```bash
forge doctor                       # Check everything
forge doctor --signal traces       # Check only trace pipeline
forge doctor --json                # Machine-readable output
forge doctor --verbose             # Show evidence for passing checks
```

## Middleware

Generated projects include a production-ready middleware stack:

| Middleware | Description |
|-----------|-------------|
| Auth | JWT and/or API key validation with per-method skip lists |
| Tenant | Claim-based tenant extraction into request context |
| RBAC (Authz) | Role-based access control from proto annotations |
| Rate Limiting | Token bucket rate limiter (per-user or per-IP) |
| Idempotency | Response caching by idempotency key (configurable TTL and cache size) |
| CORS | Configurable cross-origin resource sharing |
| Recovery | Panic recovery with structured error responses |
| Request ID | Unique request ID injection and propagation |
| Security Headers | Standard security headers (HSTS, CSP, X-Frame-Options) |
| Logging | Structured request/response logging |
| Tracing | OpenTelemetry trace handler |
| Redact | Sensitive field redaction in logs and responses |

## Testing

Forge generates multiple layers of test infrastructure:

**Unit tests** with generated mocks for every internal package contract. Run with `forge test unit`.

**Integration tests** using real database connections, tagged with `//go:build integration`. Run with `forge test integration`.

**End-to-end tests** from the `e2e/` directory with full service stack. Run with `forge test e2e`.

**CRUD lifecycle tests** are auto-generated alongside CRUD handlers — they cover create, get, list (with pagination), update, and delete flows.

All test commands support `--race` (enabled by default), `--coverage`, `--parallel`, and `--service <name>` to target specific services.

## Deployment

### KCL for Kubernetes

Instead of Helm charts or Kustomize overlays, Forge generates [KCL](https://kcl-lang.io/) configurations — a typed configuration language with schema validation. Each environment (dev, staging, prod) has its own `main.k` that composes applications from a shared schema, with compile-time validation catching misconfigurations before they reach your cluster.

```bash
forge deploy dev                   # Deploy to local k3d cluster (auto-created)
forge deploy staging --image-tag v1.2
forge deploy prod --dry-run        # Preview production manifests
```

### CI/CD

New projects include GitHub Actions workflows covering the full pipeline:

- **ci.yml** — Lint (Go, proto, TypeScript), test with race detection, build, verify generated code is committed, vulnerability scanning with `govulncheck`
- **build-images.yml** — Docker build with Trivy security scanning
- **deploy.yml** — Staging auto-deploys on main, production requires manual approval via version tag

### Infrastructure

- Docker Compose with Postgres and the full observability stack
- Multi-stage Dockerfile (CGO_ENABLED=0, stripped binary)
- k3d local cluster with local image registry at `localhost:5050`
- k6 load testing scaffold

## Documentation

- [Getting Started](docs/guides/getting-started.md) — Full walkthrough from install to deploy
- [Architecture](docs/guides/architecture.md) — Single binary design, two contract systems, explicit wiring
- [KCL Deployments](docs/guides/kcl.md) — Kubernetes configuration with KCL
- [Database & ORM](docs/guides/database.md) — Migration-first ORM, sqlc integration, migrations
- [CI/CD Pipelines](docs/guides/cicd.md) — Generated GitHub Actions workflows
- [CLI Reference](docs/guides/cli-reference.md) — Every command, flag, and option

## Contributing

Contributions are welcome. The project uses standard Go tooling:

```bash
go test ./...                      # Run tests
go build -o bin/forge ./cmd/forge  # Build the CLI
golangci-lint run ./...            # Lint
```

The codebase is organized as:

| Directory | Purpose |
|-----------|---------|
| `cmd/forge/` | CLI entrypoint |
| `cli/` | Public API for embedding Forge as a subcommand |
| `internal/cli/` | Command implementations (Cobra) |
| `internal/codegen/` | Code generation from proto and Go AST |
| `internal/templates/` | Embedded Go templates for generated files |
| `internal/packs/` | Pack system (registry, templates, install/remove) |
| `internal/generator/` | Project and service scaffolding |
| `internal/doctor/` | Environment diagnostics (`forge doctor`) |
| `internal/debug/` | Delve debugger integration |
| `internal/database/` | Migration and schema introspection |
| `internal/linter/` | DB entity lint rules |
| `internal/docs/` | Documentation generation |
| `pkg/orm/` | ORM runtime library (cursor, filter, query, repository) |
| `pkg/middleware/` | Reusable HTTP middleware (idempotency, redact) |
| `proto/forge/options/v1/` | Proto annotations for services, methods, entities, fields |

When adding a new command, add the Cobra command in `internal/cli/`, register it in `root.go`, and add corresponding templates in `internal/templates/` if the command generates files.

Please open issues for bugs and feature requests, and submit pull requests against the `main` branch.
