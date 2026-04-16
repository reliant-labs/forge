---
name: forge
description: Forge project conventions — proto-first Go/Next.js development with Connect RPC.
---
# Forge

Forge is a proto-first development framework where every service communicates via Connect RPC. Protobuf definitions are the source of truth; Go handlers, TypeScript clients, mocks, and middleware are all generated from them.

## Project layout

```
proto/services/<svc>/v1/   # Protobuf service definitions (source of truth)
gen/                       # Generated code — NEVER hand-edit, not committed
handlers/<svc>/            # Go handler implementations (one per service)
frontends/<name>/          # Next.js frontends consuming generated TS clients
internal/<name>/           # Internal Go packages with interface contracts
pkg/app/bootstrap.go       # Wires services, middleware, and packages together
pkg/middleware/             # HTTP/Connect middleware
db/migrations/             # SQL migrations (append-only history)
db/queries/                # sqlc query definitions
deploy/kcl/<env>/          # KCL deployment manifests per environment
e2e/                       # End-to-end tests (run against live stack)
forge.project.yaml         # Project config: services, ports, frontends
```

## The generate cycle

Proto drives everything. When you change a `.proto` file:

```
forge generate
```

This rebuilds `gen/` (Go stubs, Connect handlers, TypeScript clients, mocks, middleware, sqlc, bootstrap wiring). Always regenerate before running or testing after proto changes. Stale generated code is the #1 source of confusing errors.

## Dev loop

```
forge run                  # Full stack: infra + Go (hot reload) + Next.js
forge test                 # Unit + integration tests
forge test e2e             # E2E tests (requires stack running)
forge lint                 # Go + proto + frontend linters
forge build                # Binaries + frontends + Docker images
forge deploy dev           # Deploy to local k3d cluster
```

## How pieces connect

1. **Define** contracts in `proto/` — messages, RPCs, field numbers are forever
2. **Generate** with `forge generate` — fills `gen/` with typed stubs
3. **Implement** handlers in `handlers/<svc>/service.go` using generated types
4. **Consume** from frontends via generated TypeScript Connect clients
5. **Wire** everything in `pkg/app/bootstrap.go`
6. **Test** at every level: unit (mocked), integration (real DB), e2e (full stack)

## Rules

- Never hand-edit anything under `gen/`. Fix the proto or query, then regenerate.
- One service per proto package. One handler directory per service.
- Field numbers are forever — mark removed fields as `reserved`, never reuse numbers.
- `forge.project.yaml` tracks ports and services — use `forge add` to scaffold, not copy-paste.

## Sub-skills

Load sub-skills for specific actions: services, api, frontend, testing, debug, db, deploy.
