# forge — agent notes

## Engineering disposition

forge is a long-lived product (open-sourced, sold to external customers). For consequential decisions — architecture, public APIs, the KCL schema, the CLI surface, anything customers depend on — reason to the production-grade, durable solution first, not the quick fix. Before building a non-trivial design, name the realistic alternatives and the trade-off, and devil's-advocate the chosen approach **including your own proposals**; bias toward the coherent design that won't hit a ceiling. forge ORCHESTRATES and DELEGATES (build → BuildKit, releases → Helm); it must not reimplement a build system.

**Don't manufacture urgency.** There is almost never real time pressure — user frustration usually means you haven't converged on the right thing, not "go faster / cut corners." Commit to the right design and ship it decisively; never rationalize a hack with invented pressure. When tempted by a shortcut, the question is "is this correct?", not "is this faster?"

Skip this depth for trivial/mechanical work — there, just do it.

## Testing tiers

Run the cheapest tier that answers your question. Wall-clock budgets are
enforced conventions, not aspirations — if you add a test that breaks a
budget, gate it (see below).

1. **Inner loop — every edit:** `go test -short ./...` (`task test:short`)
   Whole repo in **<60s** (typically ~10s warm). This is the default for
   agents iterating on a change.
2. **Package-targeted — before committing:** `go test ./internal/<pkg>/`
   Full mode for the package you touched. `internal/cli` takes ~80s in
   full mode because the `TestRunAddFrontend_*` tests run a real
   `npm install`; everything else is seconds.
3. **Full gate — once per round / CI:** `go test -race -count=1 ./...`
   plus the e2e corpus:
   `go test -tags e2e -count=1 -timeout 60m -run TestE2E ./internal/cli/`
   That corpus is the scaffold suite (scaffold_*_e2e_test.go), the
   real-project fixture corpus (fixture_corpus_e2e_test.go), and the
   registration lifecycle (serve_types_only_e2e_test.go). The tests are
   `t.Parallel()` (independent projects in separate temp dirs, forge
   binary built once via `sync.Once`), so the gate's wall-clock is
   roughly the slowest fixture, not the sum.

Rules that keep the tiers honest:

- Any test that takes **>2s** (subprocess spawns, network, real
  scaffolds, `go build`/`go mod tidy`, `npm install`) must be skipped or
  have its slow side-effect bypassed under `testing.Short()`, with the
  slow path still exercised in full mode and CI. Never weaken an
  assertion to get under the budget — gate, don't gut.
- e2e tests that boot servers must allocate ports with
  `freePortE2E(t)` (internal/cli/scaffold_e2e_test.go) — never
  hard-code a port; the corpus runs in parallel.
- e2e tests must keep all state inside their own `t.TempDir()` project;
  no `t.Setenv`/`t.Chdir` in parallel tests (Go panics on the combo).
- CI runs the full non-short suite with `-race`; `-short` is a
  local/agent convention only.

See the comment block at the top of
`internal/cli/fixture_corpus_e2e_test.go` for the same tiers from the
e2e corpus's point of view.
