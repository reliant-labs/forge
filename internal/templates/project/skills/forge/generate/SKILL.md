---
name: forge/generate
description: Regenerate Go code from .proto files after changing the service contract.
when_to_use:
  - You added, removed, or changed a message or RPC in any .proto under proto/
  - You added a new service with `forge add service`
  - Compilation fails with missing types from gen/...
  - You just cloned the project and gen/ is empty
---

# forge/generate

Forge is proto-first. Every Connect RPC handler, DTO, and client is generated from `proto/`. The generated `.pb.go` and `.connect.go` files under `gen/` are **not committed** — they are rebuilt by `forge generate`.

## Core commands

```
forge generate            # run the full codegen pipeline
forge generate --watch    # re-run on proto/ changes (dev mode)
forge generate --force    # force regeneration of config files like buf.gen.yaml
```

The pipeline runs:
1. `buf generate` — Go stubs (protoc-gen-go + protoc-gen-connect-go)
2. `protoc-gen-forge-orm` — entity protos in `proto/db/`
3. `buf generate` — TypeScript stubs for Next.js frontends
4. Service stubs and mocks for new services
5. `pkg/app/bootstrap.go` regeneration
6. `sqlc generate` if `sqlc.yaml` exists
7. `go mod tidy` inside `gen/`

## Workflow

1. Edit the proto under `proto/services/<svc>/v1/<svc>.proto`.
2. Regenerate:
   ```
   forge generate
   ```
3. If handlers now reference new imports, tidy the root module:
   ```
   go mod tidy
   ```
   (`forge generate` already tidies `gen/`; the root module is yours.)
4. Implement the new RPC in `handlers/<svc>/service.go`. The `UnimplementedXxxHandler` embedding lets you compile with stubs while you fill it in.
5. Run tests:
   ```
   forge test --service <svc>
   ```

Tip: during a dev session with lots of proto iteration, `forge generate --watch` re-runs the pipeline on every save.

## Rules

- Never hand-edit files under `gen/`. They are overwritten on every `forge generate`. Fix the `.proto` instead.
- Generated code is **not committed**. Fresh clones must run `forge generate` before `forge run` or `forge test`. Only `gen/go.mod` and `gen/go.sum` are tracked (to keep the Go workspace consistent).
- Always regenerate before running tests if you touched any proto. Stale generated code is the #1 source of mysterious compile errors.
- Field numbers are forever. Never reuse a proto field number after removing a field — mark it `reserved`.
- Buf lint rules in `buf.yaml` are the contract. Fix lint failures by fixing the proto, not by loosening the rules.

## When this skill is not enough

- You need a code-first type that isn't proto-shaped → put it in `pkg/` or `internal/`, not in `gen/`.
- You need a Go-interface contract instead of a Connect RPC → see `forge/package` for internal packages with Go contracts.
- A proto change broke downstream handlers at runtime → use `forge/debug`.
