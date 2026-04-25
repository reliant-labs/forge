---
title: "Code Generation"
description: "Understanding Forge's infrastructure generation pipeline"
weight: 30
---

# Code Generation

Forge generates infrastructure from your contracts — proto files define the API surface, Go interfaces define internal boundaries, and `forge generate` produces the mocks, middleware wrappers, dependency wiring, and test harness that connect everything together. Business logic and database schema are developer-owned and never overwritten.

## What Gets Generated

**Regenerated on every `forge generate`:**
- Proto messages and Connect RPC stubs (via `buf generate`)
- TypeScript stubs for Next.js frontends
- Mocks for API services and internal packages
- Middleware wrappers (logging, tracing) for internal packages
- Dependency wiring (`pkg/app/wire.go`)
- Test harness (`pkg/app/testing.go`)
- sqlc query code (if `sqlc.yaml` exists)

**Scaffolded once, then developer/LLM-owned:**
- Service `service.go` stubs (non-destructive — never overwrites existing files)
- Business logic (handler implementations)
- Database schema (`db/migrations/`)
- Entity types and ORM functions (`internal/db/`)

## Generation Pipeline

```mermaid
graph LR
    A[Proto Files] --> B[buf generate]
    B --> C[Go Structs]
    B --> D[Connect RPC Stubs]
    E[contract.go] --> F[Go AST Analysis]
    F --> G[Mocks]
    F --> H[Middleware Wrappers]
    I[forge.yaml] --> J[Wiring Code]
```

## Command

```bash
# Generate all infrastructure
forge generate

# Watch mode — regenerate on contract changes
forge generate --watch
```

## Generation Steps

1. **`buf generate`** — Proto messages + Connect RPC stubs into `gen/`
2. **`buf generate`** — TypeScript stubs for Next.js frontends (if any)
3. **Service stubs** — `service.go` for new services (non-destructive)
4. **Mock generation** — Mocks for API services and internal packages
5. **Middleware wrappers** — Logging/tracing wrappers for internal packages
6. **Wiring code** — `pkg/app/wire.go` with construction logic
7. **`sqlc generate`** — If `sqlc.yaml` exists
8. **`go mod tidy`** — In `gen/`

## Generated Code Structure

```
gen/
├── go.mod
├── services/
│   └── users/v1/
│       ├── users.pb.go             # Proto messages
│       └── usersv1connect/
│           └── users.connect.go    # Connect RPC stubs
└── forge/options/v1/
    └── options.pb.go               # Proto option definitions

pkg/app/
├── wire.go                         # GENERATED — dependency construction
├── wire_test.go                    # GENERATED — test wiring
└── testing.go                      # GENERATED — test helpers

services/mocks/
└── users_service_mock.go           # Generated mock

internal/<name>/
├── mock_gen.go                     # Generated mock from contract.go
└── middleware_gen.go               # Generated logging/tracing wrapper
```

## What `forge generate` Does NOT Touch

- `services/*/handlers.go` — your business logic
- `internal/db/` — entity types, ORM functions
- `db/migrations/` — SQL migration files
- `deploy/` — Dockerfile, KCL manifests, docker-compose
- `.github/workflows/` — CI/CD pipelines

These are scaffolded once during `forge new` or `forge add service`, then owned by you. This is intentional: infrastructure is regenerable, but business logic and schema are yours to evolve.

## Contract Enforcement

The `forge lint --contract` command enforces that all exported methods match their contracts:

- For proto API services: methods must correspond to proto RPCs
- For internal packages: methods must be in the `contract.go` interface

```bash
forge lint --contract
forge lint --contract ./services/users
```

## Proto Conventions

### Package Naming

```protobuf
package services.users.v1;

option go_package = "github.com/myorg/myapp/gen/services/users/v1;usersv1";
```

### Service Definitions

```protobuf
service UsersService {
  rpc CreateUser(CreateUserRequest) returns (CreateUserResponse);
  rpc GetUser(GetUserRequest) returns (GetUserResponse);
}
```

## Buf Configuration

`buf.yaml`:
```yaml
version: v2
modules:
  - path: proto
    name: buf.build/myorg/myapp

lint:
  use:
    - STANDARD

breaking:
  use:
    - FILE
```

## Regeneration

Infrastructure is regenerated when:
- Proto files or `contract.go` files change (via `--watch`)
- Running `forge generate`
- After `forge add service` or `forge package new`

## Best Practices

1. **Define contracts first** — Proto for APIs, `contract.go` for internals, then generate
2. **Versioning** — Use `/v1`, `/v2` in proto package names
3. **Commit generated code** — Makes builds reproducible
4. **Use buf** — Leverage buf's linting and breaking change detection
5. **Don't edit generated files** — They will be overwritten on the next `forge generate`

## See Also

- [Proto Conventions]({{< ref "proto-conventions" >}})
- [Creating Services]({{< ref "../guides/creating-services" >}})
- [Database Integration]({{< ref "../guides/database-integration" >}})
