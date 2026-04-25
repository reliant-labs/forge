---
name: architecture
description: Architecture overview — project structure, generated vs hand-written code, the generate pipeline, and wiring.
---

# Architecture Overview

## Project Structure

```
cmd/                          # Application entrypoints
  server/main.go              #   Main server binary
proto/services/<svc>/v1/      # Protobuf service definitions (API contracts)
handlers/<svc>/               # Go handler implementations (YOUR business logic)
  service.go                  #   Handler methods
  authorizer.go               #   Custom authorization (yours to edit)
  authorizer_gen.go           #   Generated RBAC (regenerated — do not edit)
  handlers_crud_gen.go        #   Generated CRUD handlers (regenerated)
frontends/<name>/             # Next.js frontends
  src/app/                    #   App Router pages and layouts
  src/hooks/                  #   Generated + custom hooks
  src/lib/                    #   Utilities and Connect client setup
gen/                          # ALL generated code — NEVER hand-edit
  go/                         #   Go stubs (protoc-gen-go, protoc-gen-connect-go)
  ts/                         #   TypeScript clients
internal/db/                  # Database layer (YOU own this)
  types.go                    #   Entity types (aliases or concrete structs)
  <entity>_orm.go             #   CRUD functions per entity
  mappers.go                  #   Proto ↔ DB type mappers (when they diverge)
internal/<name>/              # Internal Go packages with interface contracts
  contract.go                 #   Interface definition
  <name>.go                   #   Implementation
pkg/app/                      # Application wiring
  bootstrap.go                #   Generated service registration — DO NOT EDIT
  setup.go                    #   Custom wiring — YOUR hook (//forge:allow)
  testing.go                  #   Test harness for integration tests
pkg/middleware/               # HTTP/Connect middleware (generated)
pkg/config/                   # Config struct + loader
db/migrations/                # SQL migrations — THE schema source of truth
db/queries/                   # SQL query definitions
deploy/kcl/<env>/             # KCL deployment manifests per environment
e2e/                          # End-to-end tests
forge.yaml                    # Project config: services, ports, frontends, packs
forge_descriptor.json         # Proto descriptor data (generated)
```

## Generated vs Hand-Written

| Forge generates (safe to regenerate) | You own (Forge never touches) |
|--------------------------------------|-------------------------------|
| `gen/` — Go stubs, TS clients, mocks | `handlers/<svc>/service.go` — business logic |
| `pkg/app/bootstrap.go` — service wiring | `pkg/app/setup.go` — custom wiring |
| `pkg/middleware/` — HTTP/Connect middleware | `internal/db/` — entity types, ORM functions |
| `handlers/<svc>/*_gen.go` — CRUD, authorizer | `handlers/<svc>/authorizer.go` — custom auth |
| Frontend hooks (`*-hooks.ts`) | `db/migrations/` — schema source of truth |
| `forge_descriptor.json` | `db/queries/` — SQL queries |
| `frontends/<name>/src/lib/connect.ts` | `internal/<pkg>/` — internal packages |

**Rule of thumb**: If it has `_gen` in the name or lives in `gen/`, it's regenerated. Everything else is yours.

## The Generate Pipeline

```
proto/services/<svc>/v1/<svc>.proto
  → protoc-gen-forge --mode=descriptor → forge_descriptor.json
  → protoc-gen-forge --mode=orm        → *.pb.orm.go (ORM code)
  → protoc-gen-go + protoc-gen-connect-go → gen/ stubs

internal/*/contract.go (Go interfaces)
  → forge generate → mock_gen.go, middleware_gen.go, tracing_gen.go

forge_descriptor.json + handlers/<svc>/
  → forge generate → handlers_crud_gen.go, authorizer_gen.go

gen/ts/ (TypeScript clients)
  → forge generate → frontends/<name>/src/hooks/*-hooks.ts
```

Running `forge generate` is always safe. It only touches infrastructure — never your handlers, DB layer, or business logic.

## Custom Wiring in setup.go

`pkg/app/bootstrap.go` is generated and auto-registers services, workers, and internal packages. **Never edit it.**

`pkg/app/setup.go` is yours. Use it to wire custom dependencies — database handles, external clients, feature flags, anything `bootstrap.go` can't know about:

```go
// pkg/app/setup.go
func Setup(app *App) error {
    // Wire custom dependencies here
    app.UserService.DB = app.Pool
    app.UserService.EmailClient = ses.NewClient(app.Config.AWS)
    return nil
}
```

`setup.go` is marked with `//forge:allow` and will never be overwritten.

## Test Harness in testing.go

`pkg/app/testing.go` provides helpers for integration tests — bootstrapping a real app with a test database, authenticated clients, and cleanup:

```go
func TestCreateUser(t *testing.T) {
    harness := app.NewTestHarness(t)
    client := harness.AuthenticatedClient(t, "user-1", "admin")
    // ... test with real DB and middleware
}
```

The harness runs migrations, seeds data, and tears down after each test.

## Files NOT to Edit

These are regenerated by `forge generate` — your changes will be overwritten:

- `gen/` — All generated Go and TypeScript code
- `pkg/app/bootstrap.go` — Service registration and wiring
- `pkg/middleware/` — HTTP/Connect middleware
- `handlers/<svc>/*_gen.go` — Generated CRUD handlers and authorizers
- `frontends/<name>/src/hooks/*-hooks.ts` — Generated React Query hooks
- `frontends/<name>/src/lib/connect.ts` — Connect transport setup
- `forge_descriptor.json` — Proto descriptor data

## Key Commands

| Command | When to use |
|---------|------------|
| `forge generate` | After any proto or contract change |
| `forge build` | Verify everything compiles |
| `forge run` | Start the full dev stack |
| `forge test` | Run all tests |

## Rules

- Never hand-edit anything under `gen/` or any `*_gen.go` file. Fix the proto or contract, then regenerate.
- `bootstrap.go` is regenerated — all custom wiring goes in `setup.go`.
- `forge generate` is always safe — it only touches infrastructure.
- One service per proto package. One handler directory per service.
- `forge generate` does NOT touch the DB layer — you own `internal/db/` and `db/migrations/`.
