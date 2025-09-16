# Forge

Proto-first application scaffolding for Go + Next.js monorepos, designed for LLM-assisted development. Forge generates complete, production-ready projects from protobuf definitions -- services, database schemas, TypeScript clients, Kubernetes deployments, and CI/CD pipelines -- with an architecture that LLMs can understand, extend, and verify.

## Why Forge

LLMs are powerful code generators, but they struggle with ambiguous architectures, sprawling codebases, and no way to confirm their output actually works. Forge solves this by generating projects that are purpose-built for AI-assisted development:

**Reduced context, better generation.** Forge projects are contract-driven. Proto files and Go interfaces define every boundary, so an LLM doesn't need to read thousands of lines of implementation to understand what a service does or how packages connect. It reads the contract, generates the implementation, and moves on. Less context consumed means higher quality output.

**Clean architecture, predictable patterns.** Every service follows the same structure: constructor injection, explicit dependency wiring, no global state, no `init()` side effects. An LLM that has seen one Forge service can generate the next one correctly. Predictable patterns eliminate the guesswork that causes LLMs to hallucinate project-specific conventions.

**Built-in feedback loops for self-verification.** This is the critical piece. LLMs need to run code to know if what they built actually works. Forge generates test harnesses, mock dependencies, contract linters, and a full dev environment out of the box. An LLM can generate a handler, run `forge test`, see the failure, fix it, and iterate -- all without human intervention. The generated CI pipeline (`forge lint --contract`, race-detected tests, build verification) gives the LLM concrete, automated checkpoints to validate its own work.

## What Forge Does

Forge is a CLI that scaffolds and manages Go + Next.js monorepo projects where protobuf files define external API contracts and Go interfaces define internal package boundaries. When you define a service in a `.proto` file, Forge generates everything that follows: Go server stubs with Connect RPC, TypeScript client code for your Next.js frontend, Kubernetes manifests via KCL, Docker images, CI/CD workflows, and end-to-end test harnesses.

The generated projects use [Connect RPC](https://connectrpc.com/) (compatible with gRPC, works over HTTP/1.1 and HTTP/2) for all external service communication. Internal package boundaries use Go interfaces defined in `contract.go` files, which are richer than proto -- supporting channels, complex types, factories, and variadic arguments.

Forge generates a complete monorepo with Go services, Next.js frontends, KCL-based Kubernetes deployments, and CI/CD pipelines. This is for teams and LLMs building production applications with multiple services and a frontend.

## Quick Start

```bash
# Install the CLI
go install github.com/reliant-labs/forge/cmd/forge@latest

# Create a new project
forge new myproject --mod github.com/example/myproject

# Enter the project and generate code from protos
cd myproject
forge generate

# Start the dev server (infrastructure + services + frontends with hot reload)
forge run

# Deploy to a local k3d cluster
forge deploy dev
```

After `forge new`, your project looks like this:

```
myproject/
├── forge.project.yaml        # Project configuration
├── cmd/                         # Single binary entrypoint (Cobra CLI)
│   ├── main.go                  # Root command + Execute()
│   ├── server.go                # server [services...] — HTTP server
│   └── version.go               # Build info
├── proto/
│   ├── services/api/v1/api.proto # Service definitions (you edit these)
│   ├── config/v1/               # Config proto — instantiation contract
│   └── forge/options/v1/      # Forge proto annotations
├── gen/                          # Generated Go + Connect stubs
├── services/
│   └── api/
│       ├── service.go            # New(deps), Register(mux, opts), Name()
│       └── handlers.go           # Business logic (you write this)
├── internal/                     # Internal packages (Go interface contracts)
│   └── <name>/
│       ├── contract.go           # Go interface — THE contract
│       ├── service.go            # Implementation
│       ├── mock_gen.go           # Generated mock
│       └── middleware_gen.go     # Generated logging/tracing wrapper
├── pkg/
│   └── app/
│       ├── wire.go               # GENERATED — constructs all boundaries from config
│       └── wire_test.go          # GENERATED — test config with mocks
├── frontends/                    # Next.js frontends (if --frontend flag used)
│   └── web/
│       └── src/gen/              # Generated TypeScript clients
├── deploy/
│   ├── kcl/                      # KCL Kubernetes manifests
│   │   ├── schema.k              # Application schema
│   │   ├── render.k              # Manifest rendering engine
│   │   ├── base.k                # Shared env vars and configs
│   │   ├── dev/main.k            # Dev environment
│   │   ├── staging/main.k        # Staging environment
│   │   └── prod/main.k           # Production environment
│   ├── Dockerfile                # Single multi-stage Dockerfile
│   ├── k3d.yaml                  # Local cluster config
│   └── docker-compose.yml        # Local infrastructure (Postgres, Redis)
├── .github/
│   └── workflows/
│       ├── ci.yml                # Lint → Test → Build → Verify → Vuln scan
│       ├── build-images.yml      # Docker build with Trivy scanning
│       └── deploy.yml            # Staging auto-deploy, prod manual approval
├── Taskfile.yml
├── buf.yaml / buf.gen.yaml
└── go.work / go.mod
```

## CLI Commands

| Command | Description |
|---------|-------------|
| `forge new <name> --mod <module>` | Create a new project. Use `--frontend <name>` to include a Next.js frontend. |
| `forge add service <name>` | Add a Go service with proto file and handler scaffold. Updates wiring in `pkg/app/wire.go`. |
| `forge add frontend <name>` | Add a Next.js frontend with Connect RPC client setup and TypeScript code generation. |
| `forge package new <name>` | Create an internal package with `contract.go` (Go interface), implementation, and generated mock/middleware. |
| `forge generate` | Generate all code from protos and contracts: Go stubs, Connect handlers, TypeScript clients, mocks, middleware wrappers, wiring code. Use `--watch` for dev mode. |
| `forge build` | Build the service binary and Docker image. Supports `--target <name>` and `--parallel`. |
| `forge run` | Start the dev environment: docker-compose infrastructure, Go binary via Air (hot reload), and Next.js frontends. |
| `forge deploy <env>` | Deploy to a Kubernetes environment using KCL manifests. For `dev`, auto-creates a k3d cluster. Supports `--dry-run` and `--image-tag`. |
| `forge test` | Run all tests (Go + frontend). Subcommands: `unit`, `integration`, `e2e`. |
| `forge lint` | Run golangci-lint, buf lint, and TypeScript linters. Use `--contract` for contract enforcement. |
| `forge db migration new <name>` | Create a new blank SQL migration pair using golang-migrate naming conventions. |
| `forge db migrate <up\|down\|status\|version\|force>` | Manage database migrations with golang-migrate. |

For complete flag documentation, see the [CLI Reference](docs/guides/cli-reference.md).

## Features

### LLM-Friendly by Design

Forge's architecture decisions directly benefit LLM-driven development:

- **Contracts as context anchors.** Proto files and `contract.go` interfaces are compact, self-contained descriptions of every boundary. An LLM reads a contract file instead of tracing through implementation code, dramatically reducing the context window needed to generate correct code.
- **One pattern, every service.** Constructor injection via `Deps` structs, `New()` constructors, and `Register()` for HTTP wiring. Once an LLM learns the pattern, it applies everywhere -- no per-service discovery required.
- **Generated mocks and test harnesses.** Every internal package gets a generated mock and every service gets a test helper. LLMs can write tests immediately without setting up infrastructure, and run them to verify their own output.
- **Contract linting as a guardrail.** `forge lint --contract` catches drift between interfaces and implementations. An LLM can run this after generating code to confirm it didn't break the contract -- a fast, deterministic feedback signal.
- **Full dev environment in one command.** `forge run` starts infrastructure, services, and frontends. An LLM doesn't need to figure out how to set up Postgres, Redis, or the frontend build -- it just runs the command and tests against a real environment.

### Two Contract Systems

Forge enforces two types of contracts depending on the boundary:

**Proto contracts for external boundaries.** Service RPCs defined in `proto/services/` and config in `proto/config/` drive Go server stubs and TypeScript clients via Connect RPC. These define the wire format that external consumers see.

**Go interface contracts for internal boundaries.** Internal packages define their contracts as Go interfaces in `contract.go`. These are richer than proto -- supporting channels, complex types, factories, and variadic arguments. Generated mocks and middleware wrappers come from Go AST analysis of these interfaces.

### Single Binary Architecture

Each project produces a single Go binary with a Cobra CLI:

```bash
myproject server                    # Start all services
myproject server api users          # Start only specific services
myproject version                   # Print build info
```

All services share one HTTP mux, one middleware stack, and one process. Dependencies are wired explicitly in the generated `pkg/app/wire.go` -- no `init()` side effects, no global registry.

### Constructor Injection

Services and internal packages receive their dependencies through `Deps` structs:

```go
type Deps struct {
    DB     *pgxpool.Pool
    Logger *slog.Logger
    Users  users.Contract  // Go interface from internal/users/contract.go
}

func New(deps Deps) *Service {
    return &Service{deps: deps}
}
```

### Contract Enforcement

The `forge lint --contract` command scans service types and reports any exported methods that don't match their contract interface. For proto API services, exported methods must correspond to proto RPCs. For internal packages, exported methods must be declared in the `contract.go` interface.

### Migration-First Database

SQL migrations in `db/migrations/` are the source of truth for schema evolution. Proto DB entities are the application contract view, validated against the migrated schema. The forge-orm library (included in this repo at `pkg/orm/`) generates thin CRUD over `database/sql` -- no heavy ORM (no GORM, no Ent). For complex queries beyond CRUD, use [sqlc](https://sqlc.dev/) alongside the generated ORM.

### Test Harness

Generated test helpers in `pkg/app/testing.go` provide `NewTestXxx` and `NewTestXxxServer` functions that wire up services with mock dependencies, making it straightforward to write integration tests without manual setup.

### KCL for Kubernetes Deployments

Instead of Helm charts or Kustomize overlays, Forge generates [KCL](https://kcl-lang.io/) configurations -- a typed configuration language with schema validation. Each environment (dev, staging, prod) has its own `main.k` file that composes applications from a shared schema, with compile-time validation catching misconfigurations before they reach your cluster. See the [KCL Guide](docs/guides/kcl.md) for details.

### Connect RPC Everywhere

All external service communication uses [Connect RPC](https://connectrpc.com/), which is wire-compatible with gRPC but also works over HTTP/1.1 with JSON. Your Go services can talk to each other via Connect, your Next.js frontend can call the same services over HTTP with full type safety, and you can debug API calls with `curl`. No gRPC-Web proxies required.

### Auto-Generated CI/CD

New projects include GitHub Actions workflows that cover the full pipeline: linting (Go, proto, TypeScript), testing with race detection, building the service binary and Docker image, verifying generated code is committed, vulnerability scanning with `govulncheck` and Trivy, and a promotion-based deploy flow where staging deploys automatically on main and production requires manual approval via a version tag. See the [CI/CD Guide](docs/guides/cicd.md).

## Documentation

- [Getting Started](docs/guides/getting-started.md) -- Full walkthrough from install to deploy
- [Architecture](docs/guides/architecture.md) -- Single binary design, two contract systems, explicit wiring
- [KCL Deployments](docs/guides/kcl.md) -- Kubernetes configuration with KCL
- [Database & ORM](docs/guides/database.md) -- Migration-first ORM, sqlc integration, migrations
- [CI/CD Pipelines](docs/guides/cicd.md) -- Generated GitHub Actions workflows
- [CLI Reference](docs/guides/cli-reference.md) -- Every command, flag, and option

### Examples

- [Full Monorepo](docs/examples/full-monorepo.md) -- 2 services + Next.js frontend with DB, CI/CD, and multi-env deploy

## Contributing

Contributions are welcome. The project uses standard Go tooling:

```bash
# Run tests
go test ./...

# Build the CLI
go build -o bin/forge ./cmd/forge

# Lint
golangci-lint run ./...
```

The codebase is organized as:

- `cmd/forge/` -- CLI entrypoint
- `internal/cli/` -- Command implementations (cobra)
- `internal/generator/` -- Project and service scaffolding
- `internal/templates/` -- Embedded templates for generated files
- `pkg/orm/` -- ORM runtime library (used by generated code)
- `proto/forge/options/v1/` -- Proto annotations for entities and fields

When adding a new command, add the cobra command in `internal/cli/`, register it in `root.go`, and add corresponding templates in `internal/templates/` if the command generates files.

Please open issues for bugs and feature requests, and submit pull requests against the `main` branch.