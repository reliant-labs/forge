# Forge

Forge is a code-generation framework and CLI for building production Go +
Next.js applications where everything communicates over
[Connect RPC](https://connectrpc.com). You describe your API once in protobuf;
Forge generates the handlers, ORM, database wiring, frontend hooks, and the
deploy manifests around it — and keeps regenerating them without clobbering
your business logic.

It is purpose-built for LLM-driven development: a single, consistent interface
pattern runs through the whole stack, so services are trivial to mock, wrap in
middleware, and swap out.

## What Forge gives you

- **Proto is the single source of truth.** API contracts, ORM models, and
  typed frontend hooks all derive from your `.proto` files via `forge generate`.
- **Generated vs. hand-written code stay separated.** Generated output lives in
  `gen/` and `*_gen.go`; your logic lives in handler files and `pkg/app/`.
  Regeneration never overwrites the code you own.
- **Migrations own the schema.** The database schema comes from SQL migrations;
  proto drives the ORM layer above them.
- **Deploy is target-agnostic.** Workloads are authored once as a KCL
  `forge.Service` and projected onto Kubernetes today (compose / host / others
  via adapters). See [`docs/design/`](docs/design/) for the design records.
- **Batteries for agents.** A rich skill catalog (`forge skill list`) encodes
  the project conventions that `forge lint` enforces.

## Install

```bash
# Build the binary into ./bin/forge
task build

# Or install onto $PATH (into $GOBIN)
task install
forge version

# Run straight from source without installing
go run ./cmd/forge version
```

## Quick start

```bash
# Scaffold a new project (service / CLI / library)
forge new my-app
cd my-app

# Add a service, then regenerate the stack from proto
forge add service billing
forge generate

# The triple gate before you call a change done:
forge generate && forge lint && go build ./... && go test ./...

# Bring the whole local dev loop up (build + deploy + host + frontend)
forge up
```

Run `forge --help` for the full command surface (`add`, `generate`, `db`,
`deploy`, `migrate`, `pack`, `mcp`, and more), or `forge <command> --help` for
any one of them.

## Conventions & skills

Forge ships an extensive skill catalog covering architecture, proto, db, api,
services, testing, frontend, deploy, and debugging. The conventions they
describe are enforced by `forge lint`, so prefer loading a skill before
guessing:

```bash
forge skill list            # discover what's available
forge skill load <name>     # read one
```

`reliant.md` at the repo root captures the critical rules and testing tiers in
brief.

## Repository layout

```
forge/
├── cmd/forge/     # CLI entrypoint (package main)
├── internal/      # CLI implementation, generators, packs, templates
├── kcl/           # KCL module: typed schemas + manifest render layer
├── pkg/           # Reusable libraries projects import (serverkit, etc.)
├── proto/         # Forge's own proto annotations (forge/v1)
├── components/    # UI component library shipped to scaffolded frontends
├── docs/          # ADRs (docs/adr) and design records (docs/design)
├── examples/      # Runnable examples
├── forge.yaml     # Project manifest
└── Taskfile.yml   # Automation entrypoints
```

## Development

```bash
task deps           # install Go (and frontend) dependencies
task test:short     # inner-loop tests: whole repo in seconds
task test           # full unit suite with -race
task lint           # golangci-lint + buf
task fmt            # goimports + go mod tidy
```

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the full dev loop, pre-commit
hooks, and PR process, and [`reliant.md`](reliant.md) for the testing tiers and
project conventions.

## License

MIT — see [`LICENSE`](LICENSE).
