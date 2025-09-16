---
name: forge/package
description: Create internal Go packages with interface contracts (for types proto can't express).
when_to_use:
  - You need a component boundary that can't be expressed in proto (channels, complex types, factories)
  - You want mock and middleware codegen for a plain Go interface
  - You're adding infrastructure like caches, notifications, or background workers
---

# forge/package

Internal packages live under `internal/<name>/` and define their boundary through a Go interface in `contract.go`. Unlike proto API services, internal-package contracts use native Go — supporting channels, complex types, factories, and anything else proto cannot express.

**This is not Docker image packaging.** `forge package new` creates Go packages. Docker image builds live under `forge build --docker`.

## Core commands

```
forge package new <name>    # create internal/<name>/ with contract.go + service.go
```

After creation, define your interface methods in `contract.go`, then:

```
forge generate              # produces mock_gen.go and middleware_gen.go for the contract
```

## Workflow

1. Create the package:
   ```
   forge package new cache
   ```
   This scaffolds:
   - `internal/cache/contract.go` — the Go interface that IS the contract
   - `internal/cache/service.go` — implementation with an unexported concrete type
2. Define your interface methods in `contract.go`. Use any Go type, including channels and generic types.
3. Regenerate:
   ```
   forge generate
   ```
   This produces `mock_gen.go` (for tests) and `middleware_gen.go` (for cross-cutting concerns like logging and metrics).
4. Implement the concrete type in `service.go`.
5. Wire the package in `pkg/app/bootstrap.go` alongside Connect services.

## Rules

- `contract.go` is the source of truth. Adding a method without regenerating leaves stale mocks and middleware.
- Do not hand-edit `mock_gen.go` or `middleware_gen.go`. They will be overwritten by the next `forge generate`.
- Package names must be valid Go package names (lowercase, letters / digits / underscores) and cannot shadow Go keywords or predeclared identifiers.
- Use internal packages for infrastructure boundaries that will be mocked heavily in tests. For simple utilities, a regular `pkg/` package without a contract is fine.

## When this skill is not enough

- You need a network-facing API → use `forge/add service` for a Connect RPC service instead.
- You need a shared library with no boundary → put it in `pkg/<name>/` as a plain Go package.
- You want to generate Docker images → `forge build --docker`.
