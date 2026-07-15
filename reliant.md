<!-- forge:version=1 -->
# forge

This is a **Forge** project managed by the `forge` CLI.

## Skills

Run `forge skill list` to discover available playbooks, and `forge skill load <name>` to read one. Available skills:

- **forge** — overall project conventions
- **getting-started** — onboarding walkthrough
- **services** — adding and editing services
- **api** — Connect RPC API patterns
- **db** — database, ORM, and migrations
- **frontend** — Next.js frontend overview
- **frontend/state** — state management (React Query, Zustand, URL)
- **frontend/patterns** — UI component patterns
- **proto** — protobuf schema conventions
- **architecture** — system architecture and layout
- **workers** — background job workers
- **auth** — authentication and authorization
- **packs** — reusable pack system
- **testing** — testing overview
- **testing/unit** — unit test patterns
- **testing/integration** — integration test patterns
- **testing/e2e** — end-to-end test patterns
- **debug** — debugging overview
- **debug/investigate** — investigation techniques
- **debug/isolate** — isolating failures
- **debug/reproduce** — reproducing bugs
- **deploy** — deployment and releases
- **observability** — logging, tracing, and metrics

## Critical Rules

1. **Never edit generated code** — `gen/` and `*_gen.go` files are overwritten by `forge generate`. Make changes in proto files instead.
2. **Proto is the canonical input** — all API contracts, ORM models, and frontend hooks derive from proto definitions.
3. **`forge generate` is safe** — it never overwrites hand-written business logic (handler files, `pkg/app/setup.go`, etc.).
4. **Migrations are the DB source of truth** — the database schema comes from migrations, not proto. Proto drives the ORM layer above them.
5. **Use `forge test`** — not raw `go test`. The CLI sets up the correct environment, test database, and flags.

## Testing tiers

Run the cheapest tier that answers your question. Wall-clock budgets are enforced conventions, not aspirations — if you add a test that breaks a budget, gate it.

1. **Inner loop — every edit:** `go test -short ./...` (`task test:short`). Whole repo in **<60s** (typically ~10s warm). Default for agents iterating on a change.
2. **Package-targeted — before committing:** `go test ./internal/<pkg>/`. Full mode for the package you touched. `internal/cli` takes ~80s in full mode because the `TestRunAddFrontend_*` tests run a real `npm install`; everything else is seconds.
3. **Full gate — once per round / CI:** `go test -race -count=1 ./...` plus the e2e corpus: `go test -tags e2e -count=1 -timeout 60m -run TestE2E ./internal/cli/`. The e2e tests are `t.Parallel()` (independent projects in separate temp dirs, forge binary built once via `sync.Once`), so the gate's wall-clock is roughly the slowest fixture, not the sum.

Rules that keep the tiers honest:

- Any test that takes **>2s** (subprocess spawns, network, real scaffolds, `go build`/`go mod tidy`, `npm install`) must be skipped or have its slow side-effect bypassed under `testing.Short()`, with the slow path still exercised in full mode and CI. Never weaken an assertion to get under the budget — gate, don't gut.
- e2e tests that boot servers must allocate ports with `freePortE2E(t)` (`internal/cli/scaffold_e2e_test.go`) — never hard-code a port; the corpus runs in parallel.
- e2e tests must keep all state inside their own `t.TempDir()` project; no `t.Setenv`/`t.Chdir` in parallel tests (Go panics on the combo).
- CI runs the full non-short suite with `-race`; `-short` is a local/agent convention only.

See the comment block at the top of `internal/cli/fixture_corpus_e2e_test.go` for the same tiers from the e2e corpus's point of view.

## Project Notes

<!-- Add project-specific context, conventions, and open questions here. -->