---
name: forge/add
description: Add new services or frontends to an existing Forge project.
when_to_use:
  - You need a new Connect RPC service in the project
  - You're adding a second Next.js frontend
  - You're unsure where a new component should live
---

# forge/add

`forge add` is the scaffolding entrypoint for growing a project after `forge new`. It creates directory structure, proto stubs, handler skeletons, and wires new pieces into `forge.project.yaml`.

The only `add` subcommands are **`service`** and **`frontend`**. For internal Go packages with interface contracts, see `forge/package`.

## Core commands

```
forge add service <name>    # new Go service under handlers/<name>/
forge add frontend <name>   # new Next.js frontend under frontends/<name>/
```

## Workflow

1. Pick a good name. Services and frontends get their name embedded in proto packages, import paths, and ports. Use kebab-case; proto names will have hyphens stripped.
2. Scaffold:
   ```
   forge add service billing
   ```
   This creates `proto/services/billing/v1/billing.proto`, `handlers/billing/`, updates `forge.project.yaml`, and assigns the next free port.
3. Regenerate code:
   ```
   forge generate
   go mod tidy
   ```
4. Implement RPCs in `handlers/<name>/service.go`. The generated file embeds `UnimplementedXxxHandler` so it compiles with stubs while you fill it in.
5. Test and run:
   ```
   forge test --service billing
   forge run --service billing
   ```

## Rules

- Use `forge add`, not manual copy-paste. Hand-scaffolded services will miss the config entry, port assignment, or import wiring.
- One service per proto package. Don't stuff multiple services into a single `.proto` file.
- Don't reuse port numbers. `forge.project.yaml` tracks assigned ports and `forge add` picks the next free one.
- `forge add middleware` does not exist. Middleware lives under `pkg/middleware/` and is wired in `cmd/server.go` by hand — add a new file and reference it in the middleware chain.

## When this skill is not enough

- You need an internal Go package with an interface contract → `forge/package`.
- You need a library, not a service → create a package under `pkg/` or `internal/` directly.
- You need a one-off script → add `cmd/<toolname>/main.go`; `forge add` is for long-running services and frontends.
- You need to rename an existing service → there's no `forge rename`. Do it manually: rename directories, update proto package, update imports, update `forge.project.yaml`, regenerate.
