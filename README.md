# Forge

Production-grade infrastructure generator for Go + Next.js applications. Forge gives you a fully productionized monorepo from day one — OpenTelemetry observability, auth middleware, Kubernetes deployments, CI/CD pipelines, generated mocks, contract enforcement, and test harnesses — so you focus on business logic, not the infrastructure that surrounds it.

## Why Forge

Most frameworks hand you a skeleton and leave you to wire up logging, auth, deployment, testing, and observability yourself. You spend weeks building infrastructure before you write a single line of business logic. And when you get it wrong — missing middleware, broken test isolation, inconsistent error handling — it compounds across every service.

Forge eliminates that gap. You get production infrastructure on day one, and `forge generate` is safe to re-run because it only regenerates infrastructure, never business logic.

**Production infrastructure from day zero.** Every generated project ships with structured logging, OpenTelemetry tracing, Prometheus metrics, continuous profiling, JWT and API key auth, RBAC, rate limiting, idempotency, CORS, security headers, graceful shutdown, and health checks. These aren't stubs — they're working, configured, and connected to a local observability stack you can query immediately.

**Architectural guardrails that scale.** Forge enforces a contract-driven architecture: Go interfaces define internal boundaries, proto definitions define external APIs, SQL migrations own the database schema. Constructor injection via `Deps` structs, explicit dependency wiring, no global state, no `init()` side effects. Every service follows the same structure, so your codebase stays consistent whether you have one service or twenty.

**LLM-optimized patterns.** Predictable structure means LLMs need less context to generate correct code. An LLM that has seen one Forge service can generate the next one correctly. Generated test harnesses, mock dependencies, and contract linters provide feedback loops that let LLMs iterate autonomously — generate, test, fix, repeat.

**Fast scaffold to working prototype.** Proto definitions get you off the ground fast — define your RPCs and entity messages, run `forge generate`, and you have a working API with CRUD handlers, tests, and typed clients. Then you evolve: business logic lives in handler files you own, database schema is driven by migrations, and entity types grow from proto aliases into concrete Go structs as your domain matures.

**Deployment pipeline included.** CI/CD via GitHub Actions, Docker multi-stage builds, Kubernetes manifests via KCL, local dev clusters with k3d, and infrastructure-as-code — all generated and configured from the start. `forge deploy dev` gets you a running cluster in minutes.

## Feature Highlights

**Full Observability Stack.** Every project ships with Grafana LGTM (Prometheus, Tempo, Loki, Pyroscope), Grafana Alloy collector, pre-built dashboards, structured logging, and OpenTelemetry tracing. Run `forge doctor` to validate everything is healthy.

**Production Auth & Security.** JWT validation (HS256/RS256, JWKS), API key auth, RBAC from proto annotations, tenant isolation from JWT claims, rate limiting, idempotency, CORS, security headers, panic recovery, and request ID propagation — all wired into the middleware stack.

**Generated Contracts, Mocks, and Middleware.** Every internal package gets a Go interface contract, a generated mock for testing, and a middleware wrapper that adds structured logging and OpenTelemetry tracing around every method call. No hand-writing mocks, no forgetting to add observability.

**Deployment & CI/CD.** GitHub Actions pipelines (lint → test → build → deploy), Docker multi-stage builds with Trivy scanning, KCL-based Kubernetes manifests with per-environment overrides, k3d local clusters, and k6 load testing scaffolds.

**Testing Infrastructure.** Generated mocks for every internal package, unit and integration test scaffolds, CRUD lifecycle tests, and end-to-end test harnesses. All tests support race detection, coverage reporting, and parallel execution.

**Migration-Driven Database.** SQL migrations are the source of truth for your database schema. Forge generates an initial migration from proto entity definitions to get you started, then you own the schema from there — adding columns, indexes, and constraints through migration files, not proto annotations.

**Scaffold CRUD to Get Started.** Define entity messages in your service proto and Forge scaffolds Create/Get/List/Update/Delete handlers with tests, ORM CRUD functions, type aliases, and an initial SQL migration. List operations include cursor-based keyset pagination, search and filtering, and soft delete support. This gets your API working fast — then you iterate on the business logic in files you own.

**Multi-Tenant Isolation.** Add a `tenant_key` annotation to an entity and Forge generates row-level tenant filtering across all ORM operations, plus middleware that extracts the tenant ID from JWT claims. No manual plumbing required.

**TypeScript Hooks.** For every RPC, Forge auto-generates TanStack React Query hooks — queries for read operations, mutations for writes — ready to use in your Next.js frontend.

**Pack System.** Install pre-built feature packs (`jwt-auth`, `clerk`, `stripe`, `twilio`, `api-key`, `audit-log`) that add real, working code — not just boilerplate stubs.

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
│   ├── services/api/v1/api.proto    # RPC + entity definitions (API contracts)
│   ├── config/v1/                   # Config proto — instantiation contract
│   └── forge/options/v1/            # Forge proto annotations
├── gen/                             # Generated Go + Connect stubs
├── handlers/
│   └── api/
│       ├── service.go               # New(deps), Register(mux), Name()
│       ├── handlers.go              # Business logic (you own this)
│       ├── handlers_crud_gen.go     # Generated CRUD scaffold
│       ├── handlers_crud_test_gen.go
│       └── authorizer_gen.go        # Generated RBAC policy
├── internal/                        # Internal packages (Go interface contracts)
│   ├── db/
│   │   ├── types.go                 # Type aliases: type User = apiv1.User
│   │   └── user_orm.go              # CRUD functions (Create, Get, List, Update, Delete)
│   └── <name>/
│       ├── contract.go              # Go interface — THE contract
│       ├── service.go               # Implementation (you own this)
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
│   └── migrations/                  # SQL migration files (source of truth for schema)
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
| `forge generate` | Generate all infrastructure code from protos and contracts. Safe to re-run — never overwrites business logic. Use `--watch` for dev mode, `--force` to regenerate config files. |
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
| `forge debug <subcommand>` | Debug a running service with Delve. Subcommands: `start`, `breakpoint`, `eval`, `goroutines`, `stop`. |
| `forge docs generate` | Generate documentation from proto definitions and Go source. Supports `--format html\|md\|json`. |

## Architecture

Forge uses three contract systems, each owning a different concern:

### Proto Definitions — API Contracts and Config

Proto files define your **external API surface** and **application configuration**. They are the onramp: define your RPCs and entity messages, run `forge generate`, and you have a working API with typed clients.

What proto produces:
- **Go server stubs** via Connect RPC (protoc-gen-go + protoc-gen-connect-go)
- **TypeScript clients and React Query hooks** via `@connectrpc/protoc-gen-connect-query`
- **RBAC authorizer** from `required_roles` annotations
- **Config proto** for service instantiation contracts
- **Initial CRUD handlers and tests** from entity messages — a scaffold to get you started

Proto is **not** the source of truth for your database schema or your internal domain types. It defines what goes over the wire.

### Go Interface Contracts — Internal Boundaries

Go interfaces in `contract.go` files define your **internal package boundaries**. These are the contracts that matter for testability, observability, and architectural consistency.

What Go contracts produce:
- **Mock implementations** for testing (method tracking, return value configuration)
- **Middleware wrappers** that add structured logging and OpenTelemetry tracing around every method call
- **Bootstrap wiring** that constructs all services with their real dependencies
- **Test helpers** that construct services with mock dependencies

Go contracts support the full richness of Go's type system — generics, channels, function types, embedded interfaces — things proto can't express.

### SQL Migrations — Database Schema

SQL migration files in `db/migrations/` are the **source of truth for your database schema**. Forge can generate an initial migration from proto entity definitions (`forge db migration new --from-proto`), but from that point forward, you own the schema through migration files.

This separation is intentional: API contracts and database schemas evolve at different rates and for different reasons. A new API field might map to an existing column. A database index has no API representation. Keeping them independent avoids the impedance mismatch that plagues "proto-for-everything" approaches.

## Schema Evolution

Entity types follow a natural lifecycle as your application matures:

### Phase 1: Proto Aliases (Day One)

When you first scaffold an entity, `internal/db/types.go` contains type aliases pointing at the generated proto types:

```go
type User = apiv1.User
```

This works great early on — your API type and your DB type are the same thing. CRUD handlers pass proto messages directly to ORM functions. Zero mapping code.

### Phase 2: Concrete Structs (As You Diverge)

As your application evolves, API and database concerns diverge. Your DB might need columns that aren't in the API (audit fields, denormalized data, internal state). Your API might return computed fields that aren't stored. At this point, you replace the alias with a concrete struct:

```go
type User struct {
    ID        string
    Email     string
    Name      string
    TenantID  string
    CreatedAt time.Time
    UpdatedAt time.Time
    // DB-only fields
    PasswordHash string
    LoginCount   int
    LastLoginAt  *time.Time
}
```

### Phase 3: Mappers (Explicit Conversion)

With concrete structs, you add mapper functions to convert between API types and DB types:

```go
func UserToProto(u *User) *apiv1.User { ... }
func UserFromProto(p *apiv1.User) *User { ... }
```

This is where the migration-driven approach pays off: your DB schema evolves through SQL migrations, your API types evolve through proto, and mappers bridge the gap explicitly. No magic, no hidden coupling.

## Communication

All service communication uses [Connect RPC](https://connectrpc.com/), which is wire-compatible with gRPC but also works over HTTP/1.1 with JSON. Go services talk to each other via Connect, the Next.js frontend calls the same services with full type safety, and you can debug API calls with `curl`. No gRPC-Web proxies required.

## Code Generation

`forge generate` produces infrastructure code from two sources. It is safe to re-run at any time — generated files are clearly marked (e.g., `*_gen.go`, `*_crud_gen.go`) and never overwrite your business logic in `handlers.go` or `service.go`.

### From Proto Definitions

- **Go server stubs** via Connect RPC (protoc-gen-go + protoc-gen-connect-go)
- **CRUD handler scaffolds and tests** from entity messages — Create, Get, List, Update, Delete implementations using ORM functions from `internal/db/`
- **Cursor-based pagination** for all List RPCs following AIP-158 conventions
- **Search and filtering** from request message fields — ILIKE for string fields, exact match for IDs, configurable ordering
- **Soft delete handling** when `soft_delete = true` is set on an entity — `deleted_at` column managed automatically in all ORM operations
- **Tenant-scoped queries** when `tenant_key` is annotated — automatic WHERE clause injection across all CRUD operations
- **RBAC authorizer** from `required_roles` annotations — mapping procedures to roles
- **TypeScript clients and React Query hooks** via `@connectrpc/protoc-gen-connect-query`
- **Config proto** for service instantiation contracts

### From Go Interface Contracts

- **Mock implementations** for testing (method tracking, return value configuration)
- **Middleware wrappers** that add structured logging and OpenTelemetry tracing around every method call
- **Bootstrap wiring** that constructs all services with their real dependencies
- **Test helpers** that construct services with mock dependencies

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

## Auth and Multi-Tenancy

Authentication and multi-tenancy are configured in `forge.yaml` and code-generated into your middleware stack.

**JWT Auth** supports HS256 and RS256 signing, JWKS endpoint discovery, configurable claim extraction, and per-method auth overrides via proto annotations. **API Key Auth** generates a validation interceptor and `KeyValidator` interface you implement against your key store. **Dual Auth** supports both JWT and API key on the same service.

**Multi-Tenant Isolation** extracts a tenant ID from JWT claims (configurable field, e.g. `org_id`) and injects it into a request-scoped context. All generated CRUD operations automatically include a tenant WHERE clause. The tenant column name is configurable in `forge.yaml`.

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
