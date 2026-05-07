---
name: cli
description: Migrate a CLI / library project to forge — --kind cli, second binaries, when contract.go isn't worth it.
---

# Migrate a CLI or Library Project

Use this skill when the existing project is a Cobra CLI binary, a code generator, an ops tool, or a pure Go library. For network-facing apps see `migration/service`. For prerequisites and the overall flow see `migration`.

## Scaffold

```bash
forge new <name>-next --kind cli --mod github.com/<owner>/<name>-next
```

`--kind cli` skips: `pkg/middleware/`, `cmd/server.go`, `deploy/`, service protos, the auto-emitted Connect-RPC service, and KCL manifests. You get:

```
cmd/<name>-next/main.go    # Cobra root command
internal/                  # Empty — drop your packages here
pkg/                       # Empty (or sub-module if you carve one)
forge.yaml                 # Project config
go.work + go.mod           # Workspace + module
```

For a pure library (no `cmd/` at all), use `--kind library`. Same skeleton minus the binary entrypoint.

## Where the entrypoint lives

`cmd/<name>-next/main.go` is the Cobra root. The display binary name is whatever `cmd/<dir>/` is named — at final cutover, rename `cmd/<name>-next/` → `cmd/<name>/` to drop the `-next` suffix from the installed binary.

The Cobra `Use:` field inside `internal/cli/root.go` (or whatever your CLI source uses) is independent of the binary name. Forge's own pattern is `Use: "forge"` even though the migration ships as `forge-next`. Adjust at cutover or leave — most users only see the binary name.

## Adding a second binary

There is **no `forge add binary` today.** To add a second entrypoint:

```bash
mkdir -p cmd/<second-binary>
# Write cmd/<second-binary>/main.go directly
# Add to go.work `use(...)` if it's its own module
```

This is a documented gap. Most "extra binary" cases (lint helpers, code generators, one-off ops tools) work fine as a hand-rolled `cmd/<name>/main.go` under the main module. If you find yourself adding three or more, consider whether they should be sub-commands of the primary CLI instead.

## Pure-utility packages — three options

For packages like `naming`, `validate`, format helpers — pure functions on no state — `forge package new` is overkill. The contract+mock+middleware/tracing/metrics generation has no test seam to mock when the package has no I/O, no time, no randomness, no external state.

Three honest options:

### (A) Skip the contract
Set in `forge.yaml`:
```yaml
contracts:
  strict: true
  allow_exported_funcs: true
  exclude:
    - internal/<utility-pkg>
```
Honest about the lack of test seam. Best for string-case conversions, pure formatters.

### (B) Wrap in a `Service` struct
Expose functions as methods on a `Service` struct, define a `Service` interface in `contract.go`. Now mockable, but verbose at every call site (`naming.New().ToPascalCase(s)` instead of `naming.ToPascalCase(s)`).

### (C) Hybrid
Keep free functions for ergonomics, ALSO expose a `Service` interface. Service methods delegate to free functions. Best of both, slightly redundant. Pick this when consumers want to mock but you don't want to break call sites.

**Decision rule:** pure utilities with no I/O / time / randomness / external state → (A). Anything that touches one of those → (B) or (C). For the full pattern (interface design, generated `*_gen.go` files, test usage), see `contracts`.

## Porting CLI commands

CLI commands map naturally to `internal/cli/` (or wherever your Cobra commands live). The forge `--kind cli` scaffold doesn't pre-emit a command tree — it gives you a Cobra root in `cmd/<binary>/main.go` and you build out from there.

Recommended: one Cobra command per file under `internal/cli/<command>.go`, mirroring the source layout. Set the contracts floor before porting:

```yaml
contracts:
  strict: true
  allow_exported_vars: false
  allow_exported_funcs: false
```

Cobra commands themselves don't need `contract.go` (they are wired into the root cmd, not consumed by other packages); the rule applies to `internal/<utility>/` and `internal/<service>/` packages they call into.

## When `forge generate` is mostly a no-op

For `--kind cli`, `forge generate` does NOT regenerate Connect-RPC stubs, frontend hooks, or `pkg/app/bootstrap.go` (none exist). It DOES still produce `mock_gen.go` for any `internal/<name>/contract.go` it finds. (Pre-1.7 forge also emitted `middleware_gen.go`, `tracing_gen.go`, `metrics_gen.go` per package; those wrappers were removed when observability moved to the Connect interceptor layer in `forge/pkg/observe`.)

Run `forge generate` after every contract-bearing package port. Skipping it means stale mocks; tests against your interface compile against the previous shape.

## Sub-module under `pkg/`

Carve out a `pkg/` sub-module (its own `pkg/go.mod`) when you have **runtime libraries** that downstream consumers should be able to import without pulling in your CLI / build-tool dep graph (Cobra, codegen libs, Delve, etc.). Symptoms that indicate this:

- Tooling deps bleeding into `go.sum` of importers.
- Wanting independent versioning for the runtime surface.

Setup:
```bash
# Drop a fresh go.mod at pkg/
cd pkg && go mod init github.com/<owner>/<name>-next/pkg
# Copy only the deps the runtime libraries actually use (don't inherit
# the parent's entire require block).
```

If the parent already has `go.work`, add `pkg` to the `use (...)` block. Otherwise put `replace github.com/<owner>/<name>-next/pkg => ./pkg` in the parent `go.mod`. **Don't do both** — workspace `use` supersedes `replace` and they're equivalent for build resolution.

Every directory physically under `pkg/` becomes part of the sub-module the moment `pkg/go.mod` exists. Any scaffold stubs already placed under `pkg/` (e.g. `pkg/config`) get absorbed. If a sub-package needs to stay in the main module, move it out of `pkg/` BEFORE introducing the sub-module — there's no clean way to exclude a subdir.

## Final checks before declaring done

```bash
forge generate          # mocks / middleware wrappers for any contract.go
forge lint              # contract + general lints
go build ./...          # main module + sub-modules in the workspace
go test ./...
go install ./cmd/<name>-next
$(go env GOPATH)/bin/<name>-next --help    # smoke
```

## Rules

- Use `--kind cli` (or `--kind library`) at scaffold time. Don't try to disable server-shaped emission post-hoc.
- Hand-roll second binaries under `cmd/<name>/main.go`. No `forge add binary` exists.
- For pure-utility packages, pick option (A), (B), or (C) explicitly. Don't blanket-apply `forge package new`.
- `forge generate` is mostly a no-op for `--kind cli`, but contract-bearing packages still need it for mocks/middleware.
- `pkg/` sub-module: workspace `use` OR `replace`, not both.

## When this skill is not enough

- **Server-shaped projects** — see `migration/service`.
- **Designing the interface in `contract.go`** for non-trivial packages — see `contracts`.
- **Pre-flight, module path strategy, halt-and-report rule on forge bugs** — see `migration`.
