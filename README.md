# Forge

An opinionated codebase management system for Go applications. Forge manages your project's architecture, contracts, code generation, testing infrastructure, observability, and deployment — so you (and your LLM) focus on business logic.

## Why Forge

You start a new Go project. Now you need auth middleware, observability, deployment pipelines, test harnesses, contract enforcement, database migrations, CI/CD. That's weeks of infrastructure work before your first feature ships.

Or: your LLM generates a service. It compiles. But there's no tracing, no auth, no contract interface, no mocks, no test harness, no deployment manifest. Now multiply that by 10 services.

Forge handles all of this from day one. Not as stubs. As working, tested, production-grade infrastructure.

**Forge is not a scaffolding tool you use once and throw away.** It's a long-lived integration that manages your project through its entire lifecycle:

- `forge generate` regenerates infrastructure without touching business logic — safe to run forever
- `forge upgrade` keeps your project's templates current with the latest Forge version
- `forge doctor` validates your entire observability stack end-to-end
- `forge debug` gives you an integrated Delve debugger with session persistence
- Skills teach your LLM exactly how your project works

Every feature is opt-in. Start with what you need, toggle more on in `forge.yaml` as you grow. ORM, CI, deploy, contracts, docs, frontend, observability, hot reload — each independently toggleable.

## Philosophy

**Excel where LLMs don't, step back where LLMs are good.**

LLMs are great at writing business logic, handlers, and domain code. They're bad at:

- Maintaining consistent architecture across 20 services
- Remembering to add tracing to every new internal package
- Enforcing contract interfaces and generating matching mocks
- Wiring up observability, auth, rate limiting, and deployment correctly
- Keeping generated code in sync with proto definitions

Forge handles all the structural work. LLMs write business logic inside the guardrails Forge provides. The result: an LLM that has seen one Forge service can generate the next one correctly, because the structure is predictable and the boundaries are enforced.

**A complete feedback loop.** Forge doesn't just generate code — it closes the loop between writing, testing, debugging, and observing. An LLM can write a handler, run `forge generate` to produce mocks and wiring, run `forge test` to see failures, use `forge debug` to set breakpoints and evaluate expressions (with `--json` output it can parse), query traces and logs via `forge doctor` or the Grafana MCP server, fix the issue, and iterate — all without human intervention. Every step is CLI-driven, machine-readable, and documented by skills that teach the LLM how your specific project works.

**Proto-first contracts.** API surfaces are defined in proto. Everything — handlers, ORM, TypeScript clients, mocks, middleware — is derived from those definitions. Proto annotations control auth requirements, RBAC roles, entity behavior, field validation, config binding, and more.

**Go interface contracts.** Internal package boundaries use Go interfaces in `contract.go` files. Forge auto-generates four files from each contract: mocks for testing, middleware wrappers with structured logging, OpenTelemetry tracing wrappers, and metrics instrumentation. The bootstrap wires everything through constructor injection via `Deps` structs — so swapping a real implementation for a mock, or replacing an auth provider with a different one, is just changing what gets passed to `New()`. The full richness of Go's type system — generics, channels, function types, embedded interfaces — is supported.

**Convention over configuration.** Every Forge project follows the same directory layout, naming conventions, and patterns. This isn't just for humans — it's for LLMs. Predictable structure means less context needed, which means better code generation.

**Opt-in everything.** Every feature is toggleable in `forge.yaml`. Start with a bare service, add ORM when you need persistence, CI when you're ready to ship, observability when you need to debug production. Nothing is forced on you.

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

## Project Structure

```
myproject/
├── forge.yaml                       # Project configuration (feature toggles, auth, tenancy)
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
├── .github/workflows/               # CI/CD (lint → test → vuln scan → build → deploy)
├── Taskfile.yml
├── buf.yaml / buf.gen.yaml
└── go.work / go.mod
```

## What You Get From Day One

### Production Infrastructure

- **Full observability stack** — Grafana, Prometheus, Tempo, Loki, Pyroscope, Alloy with auto-generated dashboards. Run `forge doctor` to validate every signal.
- **Auth middleware** — JWT validation (HS256/RS256/ES256), JWKS endpoint discovery, API key auth, dual auth mode, per-method auth overrides via proto annotations.
- **RBAC** — Role-based access control generated from `required_roles` proto annotations.
- **Multi-tenant isolation** — Tenant ID extraction from JWT claims, automatic WHERE clause injection across all ORM operations, compile-time enforcement via typed context.
- **Rate limiting** — Token bucket rate limiter, per-user or per-IP.
- **Idempotency** — Response caching by idempotency key with configurable TTL and cache size.
- **Security hardening** — CORS, security headers (HSTS, CSP, X-Frame-Options), panic recovery, request ID propagation, sensitive field redaction in logs.
- **Structured logging** — slog with OpenTelemetry trace correlation and log redaction for sensitive fields.
- **OpenTelemetry tracing** — Distributed tracing across all services, auto-instrumented via generated middleware wrappers.
- **Continuous profiling** — pprof endpoint exposed on every service, Pyroscope collection in the observability stack.
- **Graceful shutdown** — Configurable pre-stop delay, clean connection draining.

### Testing Infrastructure

- **Generated mocks** for every internal package contract — method tracking, return value configuration.
- **Unit, integration, and E2E test scaffolds** — tagged builds, separate test commands.
- **CRUD lifecycle tests** auto-generated alongside handlers — Create, Get, List (with pagination), Update, Delete flows.
- **Test harness** with mock dependency injection via generated `testing.go` helpers.
- **Race detector** enabled by default.
- **Multi-tenant test isolation** helpers.

### Deployment Infrastructure

- **GitHub Actions CI/CD** — lint → test → vuln scan → build → deploy. Staging auto-deploys on main, production requires manual approval via version tag.
- **Docker multi-stage builds** with Trivy security scanning.
- **KCL Kubernetes manifests** — typed configuration language with per-environment configs (dev/staging/prod) and compile-time validation.
- **k3d local cluster** with registry at `localhost:5050`.
- **Vulnerability scanning** — govulncheck, Trivy, npm audit.

### Developer Experience

- **Hot reload** via Air — Go services rebuild on file change.
- **Integrated Delve debugger** — `forge debug start` builds with debug symbols and launches under Delve. Set breakpoints by file:line or function name (with short-name resolution), evaluate arbitrary expressions, inspect locals/args/goroutines/stack traces, step through execution. Session state persists to `.forge/debug-session.json` — breakpoints survive restarts, and subsequent commands reconnect automatically. Every command supports `--json` output for machine consumption.
- **Three debug modes** — launch a service (`forge debug start api`), attach to a running process (`--attach <pid>`), or debug inside Docker (`--docker` via `docker compose --profile debug`).
- **Debug methodology skills** — LLMs get structured playbooks for triage, parallel investigation (researcher + tester + reproducer), top-down bisection from e2e to unit test, and runtime evidence collection.
- **`forge run --debug`** — starts your dev environment under Delve with hot reload, listening on `:2345` for VS Code or `dlv connect`.
- **`forge doctor`** — health diagnostics for Docker containers, app health, pprof, Prometheus, Tempo, Loki, Pyroscope, and active Delve sessions.
- **`forge upgrade`** — template drift detection and update from latest Forge version.
- **Watch mode** — `forge generate --watch` for auto-regeneration on proto or contract changes.

## Forge Manages Your Project

Forge stays with your project through its entire lifecycle.

| What | Command | When |
|------|---------|------|
| Create a project | `forge new` | Day zero |
| Add a service, worker, operator, frontend, webhook | `forge add ...` | Growing the project |
| Regenerate infrastructure from contracts | `forge generate` | After any proto or contract change |
| Run your dev environment | `forge run` | Daily development |
| Build everything | `forge build` | Before deploy |
| Run tests | `forge test` | Continuous |
| Lint everything | `forge lint` | Continuous |
| Debug a running service | `forge debug` | Investigating issues |
| Check observability health | `forge doctor` | When something seems wrong |
| Deploy to any environment | `forge deploy` | Shipping |
| Update frozen templates | `forge upgrade` | Keeping current |
| Database migrations | `forge db migrate` | Schema changes |
| Install feature packs | `forge pack install` | Adding capabilities |
| Generate documentation | `forge docs generate` | Documentation |

## What Forge Generates vs What You Own

| File | Generated | You Own | Re-generated on `forge generate` |
|------|-----------|---------|-----------------------------------|
| `handlers.go` | Initial scaffold | Yes — your business logic | Never overwritten |
| `service.go` (internal) | Initial scaffold | Yes — your implementation | Never overwritten |
| `contract.go` | Initial scaffold | Yes — your interfaces | Never overwritten |
| `*_gen.go` | Always | No | Yes — always regenerated |
| `*_crud_gen.go` | Always | No | Yes — always regenerated |
| `handlers_crud_test_gen.go` | Always | No | Yes — always regenerated |
| `authorizer_gen.go` | Always | No | Yes — always regenerated |
| `mock_gen.go` | Always | No | Yes — always regenerated |
| `middleware_gen.go` | Always | No | Yes — always regenerated |
| `bootstrap.go` | Always | No | Yes — always regenerated |
| `testing.go` (pkg/app) | Always | No | Yes — always regenerated |
| `db/migrations/*.sql` | Initial from proto | Yes — you own schema | Never overwritten |
| `proto/*.proto` | Initial scaffold | Yes — your API contract | Never overwritten |
| `forge.yaml` | Once | Yes — your config | Only with `--force` |

**The rule:** files ending in `_gen.go` or `_gen.ts` are Forge's. Everything else is yours.

## Feature Deep-Dives

### Code Generation

`forge generate` runs a 30+ step pipeline that produces infrastructure from two contract sources:

**From proto definitions:** Go server stubs (Connect RPC), CRUD handler scaffolds with tests, cursor-based keyset pagination (AIP-158), search and filtering, soft delete handling, tenant-scoped queries, RBAC authorizer, TypeScript clients, React Query hooks, config loader, ORM codegen, migration auto-generation, seed data, frontend CRUD pages, nav/dashboard generation.

**From Go interface contracts:** Mock implementations, middleware wrappers (logging + tracing), bootstrap wiring, test helpers with mock dependency injection.

**Post-generation:** sqlc codegen, CI/CD workflows, infrastructure files, Grafana dashboards, mock data/transport, `go mod tidy`, `goimports`, build verification.

Generated files are checksum-tracked. `forge generate` is safe to run at any time — it never overwrites files you own.

### Contract System

Forge uses two complementary contract systems:

**Proto contracts** define external API surfaces — RPCs, entity messages, config bindings. Proto annotations control auth requirements (`auth_required`), idempotency (`idempotent`), timeouts, RBAC roles (`required_roles`), entity behavior (`soft_delete`, `timestamps`, `tenant_key`, `indexes`), field constraints (`unique`, `immutable`, `default`, `validation`, `references`), and config binding (`env_var`, `flag`, `default_value`, `required`).

**Go interface contracts** define internal package boundaries in `contract.go` files. Every interface produces a mock, a middleware wrapper, and bootstrap wiring. This separation keeps API evolution and internal architecture independent.

**SQL migrations** own the database schema. Forge generates an initial migration from proto entities (`forge db migration new --from-proto`), then you own schema evolution through migration files. API contracts and database schemas evolve at different rates — keeping them independent avoids impedance mismatch.

### Config System

Service configuration is defined in proto (`config/v1/`), producing a typed config struct with environment variable and flag binding. Every config field has a declared type, default value, description, and required flag — no more strconv scattered through main.go.

### Component Types

| Component | Command | Description |
|-----------|---------|-------------|
| Service | `forge add service <name>` | HTTP service with proto file, handler scaffold, middleware, wiring. Optional: `--port`. |
| Worker | `forge add worker <name>` | Background worker. Use `--kind cron --schedule "..."` for cron-scheduled workers. |
| Operator | `forge add operator <name>` | Kubernetes operator. Optional: `--group`, `--version`. |
| Frontend | `forge add frontend <name>` | Next.js web (default) or React Native (Expo) via `--kind mobile`. |
| Webhook | `forge add webhook <name>` | Webhook endpoint for a service. Requires `--service`. |
| Internal Package | `forge add package <name>` | Internal package with contract interface, implementation, mock, and middleware wrapper. |

### Frontend

Forge generates full-stack frontends with typed API integration:

- **Next.js** (web) and **React Native** (Expo) via `--kind mobile`
- **Connect RPC clients** with full type safety from proto definitions
- **React Query hooks** auto-generated for every RPC — queries for reads, mutations for writes
- **CRUD pages** scaffolded from entity definitions
- **Event bus** for cross-component communication
- **Zustand stores** for client-side state
- **Auth provider** with dependency injection
- **Component library** — 60+ components across 5 categories (data display, forms, layout, navigation, feedback)
- **Mock transport** for frontend testing without a running backend
- **Tailwind v4** styling

### Pack System

Packs add real, working code to your project — not boilerplate stubs. Each pack renders templates with your module path, adds Go dependencies, and records itself in `forge.yaml`.

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
forge pack remove stripe           # Remove a pack
```

### Database & ORM

- **Migration-first** — SQL migrations are the source of truth for your database schema
- **Initial migration from proto** — `forge db migration new --from-proto` bootstraps your schema
- **LLM-context migration scaffolding** — `forge db migration new` generates migration files with schema context for LLM assistance
- **Live introspection** — `forge db introspect` inspects your running database
- **Proto sync** — `forge db proto sync-from-db` and `forge db proto check` keep proto and schema aligned
- **sqlc integration** — type-safe SQL queries alongside ORM CRUD
- **ORM codegen** — Create, Get, List, Update, Delete with cursor-based pagination, search, filtering, soft delete, tenant scoping
- **Seed data** generation for development
- **PostgreSQL + SQLite** support

### Observability

Every project ships with a complete observability stack in Docker Compose:

| Component | Role |
|-----------|------|
| Prometheus | Metrics collection |
| Tempo | Distributed tracing |
| Loki | Log aggregation |
| Pyroscope | Continuous profiling |
| Grafana | Dashboards (auto-generated: Overview, Logs, Traces) |
| Grafana Alloy | Unified collector |

Services emit structured log events, OpenTelemetry traces, and Prometheus metrics out of the box. `forge doctor` validates the full pipeline:

```bash
forge doctor                       # Check everything
forge doctor --signal traces       # Check only trace pipeline
forge doctor --json                # Machine-readable output
forge doctor --verbose             # Show evidence for passing checks
```

### Auth & Security

| Feature | Details |
|---------|---------|
| JWT validation | HS256, RS256, ES256 signing algorithms |
| JWKS | Endpoint discovery for key rotation |
| API key auth | `KeyValidator` interface, key store migration |
| Dual auth | JWT + API key on the same service |
| RBAC | Generated from `required_roles` proto annotations |
| Multi-tenant isolation | Tenant ID from JWT claims, automatic ORM filtering, compile-time enforcement |
| Rate limiting | Token bucket, per-user or per-IP |
| Idempotency | Response caching by key, configurable TTL |
| CORS | Configurable origins, methods, headers |
| Security headers | HSTS, CSP, X-Frame-Options, X-Content-Type-Options |
| Panic recovery | Structured error responses, stack trace logging |
| Log redaction | Sensitive field masking in structured logs |
| Webhook verification | Signature verification for incoming webhooks |

### CI/CD

Generated GitHub Actions workflows cover the full pipeline:

- **ci.yml** — Lint (Go, proto, TypeScript), test with race detection, build, verify generated code is committed, vulnerability scanning with `govulncheck`
- **build-images.yml** — Docker build with Trivy security scanning
- **deploy.yml** — Staging auto-deploys on main, production requires manual approval via version tag

### Deployment

**KCL Kubernetes manifests** instead of Helm or Kustomize — a typed configuration language with schema validation and compile-time error checking. Per-environment configs (dev/staging/prod) compose from a shared schema.

```bash
forge deploy dev                   # Deploy to local k3d cluster (auto-created)
forge deploy staging --image-tag v1.2
forge deploy prod --dry-run        # Preview production manifests
```

Infrastructure includes Docker Compose (Postgres + observability), multi-stage Dockerfile (CGO_ENABLED=0, stripped binary), k3d local cluster with registry, and k6 load testing scaffold.

### Debugger

Forge wraps Delve into a stateful CLI that makes debugging a first-class part of development — not an afterthought you configure manually.

**Three start modes:**
```bash
forge debug start api              # Build with -gcflags=all=-N -l, launch under Delve
forge debug start --attach 12345   # Attach to a running process by PID
forge debug start --docker         # Debug inside a Docker container (compose profile)
forge run --debug                  # Hot-reload dev environment with Delve on :2345
```

**Full debugging workflow:**
```bash
forge debug break handler.go:42              # Breakpoint at file:line
forge debug break --func handleCreate        # Breakpoint on function (short name resolved via go.mod)
forge debug break handler.go:42 --cond "id > 5"  # Conditional breakpoint
forge debug continue                         # Resume until next breakpoint
forge debug step                             # Step over current line
forge debug stepin                           # Step into function call
forge debug stepout                          # Step out of current function
forge debug eval "req.UserID"                # Evaluate any Go expression
forge debug locals                           # List local variables with types and values
forge debug args                             # List function arguments
forge debug stack                            # Print full stack trace
forge debug goroutines                       # List all goroutines with status and location
forge debug breakpoints                      # List all breakpoints with hit counts
forge debug clear 3                          # Clear breakpoint by ID
forge debug stop                             # End the session
```

**Session persistence:** Debug state is saved to `.forge/debug-session.json`. You can `forge debug stop`, modify code, rebuild, `forge debug start` again, and your breakpoints reconnect. This means LLMs can run multi-step debug sessions across tool calls without losing context.

**Machine-readable output:** Every debug command supports `--json` for structured output — breakpoints, stop states, variables (with children), stack frames, and goroutine info all serialize cleanly. This is what makes `forge debug` usable by LLM agents: they can set a breakpoint, continue, evaluate an expression, and parse the result programmatically.

**Integrated debug skills:** Forge ships with debug methodology playbooks (triage → parallel investigation → synthesis) that teach LLMs to spawn parallel agents for research, test isolation (top-down bisection from e2e to unit), and runtime reproduction with diagnostic logging — all using forge's own tools.

```bash
forge skill load debug             # Debug methodology overview
forge skill load debug/isolate     # TDD-style bug isolation via bisection
forge skill load debug/reproduce   # Runtime evidence collection
```

### The Feedback Loop

Forge is designed so an LLM can autonomously write, test, debug, observe, and fix code. Every step is CLI-driven and machine-readable.

```
┌─────────────────────────────────────────────────────────────────┐
│  Write handler      forge generate       forge test            │
│  (LLM writes code)  (mocks, wiring,      (unit, integration,  │
│                      middleware auto-     e2e — failures in    │
│                      generated from       stdout)              │
│                      contract.go)                              │
│       ▲                                         │              │
│       │                                         ▼              │
│  Fix the issue      forge doctor          forge debug          │
│  (targeted fix —    (query Prometheus,    (set breakpoints,    │
│   contract.go       Tempo, Loki —         eval expressions,   │
│   tells the LLM     verify traces/logs    inspect goroutines   │
│   the full API      are flowing)          — all with --json)   │
│   surface)                                                     │
└─────────────────────────────────────────────────────────────────┘
```

What makes this work:

- **contract.go → 4 generated files** — Write one interface, get mocks, logging middleware, tracing wrappers, and metrics instrumentation. The LLM never hand-writes mocks or forgets observability.
- **Constructor injection via `Deps` structs** — Every component is wired through interfaces in `bootstrap.go`. Swapping an auth provider, replacing a database layer, or injecting a mock is just changing what gets passed to `New()`. The LLM understands the app's configuration because `reliant-forge.md`, `forge_descriptor.json`, and 15+ skills document the full architecture.
- **E2E tests wired from scaffold** — `forge test e2e` runs against the live stack with generated test harnesses, Docker Compose infra, and per-service Connect RPC client helpers.
- **Machine-readable debugging** — `forge debug` with `--json` on every command lets LLMs set breakpoints, step through code, evaluate expressions, and inspect goroutines programmatically. Session persistence means multi-step debug workflows across tool calls.
- **Observability queries** — `forge doctor` validates the full telemetry pipeline (Prometheus, Tempo, Loki, Pyroscope). Enable the Grafana MCP server from `.mcp.json.example` and LLMs can query metrics, traces, logs, and dashboards directly.
- **Debug methodology skills** — Structured playbooks for triage, parallel investigation (researcher + tester + reproducer agents), top-down bisection from e2e to unit test, and runtime evidence collection.

### Skills & LLM Context

Forge provides structured context so LLMs understand your project:

- **15+ skills** — playbooks covering services, handlers, migrations, debugging, testing, auth, deployment, and more. Load with `forge skill load`.
- **reliant.md generation** — auto-generated project context file loaded by LLM assistants on every interaction.
- **forge_descriptor.json** — machine-readable project introspection (services, entities, configs, feature flags) generated from proto annotations.
- **MCP config** — Model Context Protocol configuration for Chrome DevTools (frontend debugging), Grafana (observability queries), and documentation search.
- **LLM-context migrations** — `forge db migration new` embeds current schema, proto models, previous migrations, and schema diffs so LLMs can write correct SQL.
- **Contract interfaces as API surface** — a single `contract.go` file tells an LLM everything it needs to know about a package's capabilities.

## CLI Reference

### Project Lifecycle

| Command | Description |
|---------|-------------|
| `forge new <name> --mod <module>` | Create project. Optional: `--service`, `--frontend`, `--license`. |
| `forge generate` | Regenerate infrastructure. `--watch` for dev mode, `--force` for config files. |
| `forge build` | Build Go binary and frontends. `--docker`, `--target`, `--debug`. |
| `forge run` | Start dev environment (Docker Compose, Air hot reload, frontends). `--debug` for Delve. |
| `forge deploy <env>` | Deploy to Kubernetes. `--dry-run`, `--image-tag`. |
| `forge upgrade` | Update project from latest templates. `--check`, `--force`. |

### Components

| Command | Description |
|---------|-------------|
| `forge add service <name>` | Add service. `--port`. |
| `forge add worker <name>` | Add worker. `--kind cron --schedule "..."`. |
| `forge add operator <name>` | Add Kubernetes operator. `--group`, `--version`. |
| `forge add frontend <name>` | Add frontend. `--kind mobile` for React Native. |
| `forge add webhook <name>` | Add webhook. `--service` (required). |
| `forge add package <name>` | Add internal package with contract. |
| `forge component list` | List available UI components. |
| `forge component search <q>` | Search components. |
| `forge component install <name>` | Install a component into your frontend. |

### Testing & Quality

| Command | Description |
|---------|-------------|
| `forge test unit` | Run unit tests. `--race`, `--coverage`, `--parallel`, `--service`. |
| `forge test integration` | Run integration tests. |
| `forge test e2e` | Run end-to-end tests. |
| `forge lint` | Run all linters. `--contract`, `--db`, `--exported-vars`. |

### Database

| Command | Description |
|---------|-------------|
| `forge db migration new <name>` | Create migration. `--from-proto` for proto-based. |
| `forge db migrate up` | Apply migrations. |
| `forge db migrate down` | Roll back migrations. |
| `forge db migrate status` | Show migration status. |
| `forge db introspect` | Inspect live database schema. |
| `forge db proto sync-from-db` | Sync proto from database schema. |
| `forge db proto check` | Check proto/schema alignment. |
| `forge db codegen` | Generate ORM code from schema. |

### Debugging & Diagnostics

| Command | Description |
|---------|-------------|
| `forge debug start` | Start debugger. `--attach`, `--docker`. |
| `forge debug break <loc>` | Set breakpoint. |
| `forge debug continue/step/stepin/stepout` | Execution control. |
| `forge debug eval <expr>` | Evaluate expression. |
| `forge debug locals/args/stack/goroutines` | Inspect state. |
| `forge debug breakpoints/clear/stop` | Manage session. |
| `forge doctor` | Health diagnostics. `--signal`, `--json`, `--verbose`. |

### Packs & Docs

| Command | Description |
|---------|-------------|
| `forge pack list` | List available packs. |
| `forge pack install <name>` | Install a pack. |
| `forge pack remove <name>` | Remove a pack. |
| `forge docs generate` | Generate documentation. |
| `forge skill list` | List available skills. |
| `forge skill load <name>` | Load a skill. |
| `forge version` | Print Forge version. |

## Communication

All service communication uses [Connect RPC](https://connectrpc.com/) — wire-compatible with gRPC but also works over HTTP/1.1 with JSON. Go services talk to each other via Connect, frontends call the same services with full type safety, and you can debug API calls with `curl`. No gRPC-Web proxies required.

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

| Directory | Purpose |
|-----------|---------|
| `cmd/forge/` | CLI entrypoint |
| `cli/` | Public API for embedding Forge as a subcommand |
| `internal/cli/` | Command implementations (Cobra) |
| `internal/codegen/` | Code generation from proto and Go AST |
| `internal/templates/` | Embedded Go templates for generated files |
| `internal/packs/` | Pack system (registry, templates, install/remove) |
| `internal/generator/` | Project and service scaffolding |
| `internal/doctor/` | Environment diagnostics |
| `internal/debug/` | Delve debugger integration |
| `internal/database/` | Migration and schema introspection |
| `internal/linter/` | DB entity lint rules |
| `internal/docs/` | Documentation generation |
| `pkg/orm/` | ORM runtime library |
| `pkg/middleware/` | Reusable HTTP middleware |
| `proto/forge/options/v1/` | Proto annotations |

Please open issues for bugs and feature requests, and submit pull requests against the `main` branch.