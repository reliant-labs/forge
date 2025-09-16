# Forge Linter - Proto Method Enforcement

> **DEPRECATED**: This document describes the old `protomethod` linter. The current architecture uses `forge lint --contract` which enforces two contract systems:
> - **Proto API services**: Exported methods on handler types must match proto RPC definitions
> - **Internal packages**: Exported methods on service types must match the Go interface in `contract.go`
>
> Exported types, functions (non-methods), constants, and variables are unrestricted.
>
> See [Contract Enforcement](docs/content/docs/architecture/code-generation.md) for current documentation.

## Overview (Historical)

The `protomethod` linter ensured that **ALL exported functions and methods** implement proto service interfaces. This has been replaced by `forge lint --contract` which is more nuanced:

- **Exported methods on service types** must match the contract interface (proto RPC or `contract.go`)
- **Exported types, functions, constants, variables** are unrestricted — data types, constructors (`New`), parsers, utilities, sentinel errors are all allowed
- **Two contract types**: proto for external boundaries, Go interfaces for internal boundaries

The rationale: methods on the service type are the operational boundary where middleware wraps, mocking happens, and logging/tracing intercepts. Everything else is just data and helpers.

## Current Usage

```bash
# Run contract enforcement
forge lint --contract

# Checks:
# - services/<name>/: exported methods match proto RPC definitions
# - internal/<name>/: exported methods match contract.go interface
# - Exported types, functions, constants, variables: unrestricted
```
