---
name: testing
description: "Testing methodology — the test pyramid, mock vs real, harness patterns, flakiness prevention, and discipline."
emit: both
---

# Testing Methodology

## The Test Pyramid

Structure tests in layers, with volume decreasing as scope increases:

1. **Unit** — fast, hermetic, single-component. The default; runs in seconds.
2. **Integration** — touches a real dependency (database, file system, child process). Runs in tens of seconds.
3. **End-to-end** — full stack, multi-component flows. Runs in minutes.

The pyramid shape matters. Many units, fewer integrations, a small e2e set. Inverting it — lots of e2e, few units — makes the suite slow and brittle, and failures point at the wrong layer.

## Mock vs Real Decision Framework

This is the most critical testing decision. Get it wrong and tests either break constantly or miss real bugs.

### Keep REAL

- **The thing under test** — never mock what you're testing.
- **Fast, deterministic things** — pure functions, in-memory data structures, value objects.
- **Things where mocking hides bugs** — ORM behavior, SQL queries, serialization logic.

### MOCK

- **External services** — third-party APIs, email providers, payment gateways.
- **Non-deterministic sources** — time, random numbers, network calls.
- **Slow irrelevant resources** — services not related to the behavior under test.

### The Boundary Rule

Mock at **system boundaries**, keep internals real. If two components live in the same service, test them together. If they cross a network boundary, mock the far side.

## Test Harness Patterns

- **Test database** — transaction-per-test with rollback for isolation. Each test starts with a known fixture, mutates freely, and the rollback wipes its state without contaminating the next.
- **External services** — mock at the typed client interface, not at HTTP. Mocking HTTP forces every test to know your serialization shape; mocking the client lets the test express intent.
- **Authenticated client** — build a helper that returns a pre-authenticated client for the test's chosen user; don't reinvent the auth flow per-test.
- **Fixtures** — deterministic seed data, created in setup, cleaned in teardown. No "leftover from a previous run" allowed.

## Flakiness Prevention

Flaky tests erode trust. Follow these rules strictly:

- **Never use fixed sleeps.** `sleep(2)` to "wait for the worker" is always wrong. Poll with a timeout instead.
- **Poll with a deadline.** For async operations, retry a check until it passes or a deadline expires. Fail with the last observed state, not just "timed out."
- **Event-driven waiting.** Channels, condition variables, or test hooks beat polling when the runtime supports them — cheaper and more responsive.
- **Deterministic seeds.** If randomness is involved, seed it explicitly and log the seed so a failure reproduces.
- **Isolate state per test.** No shared mutable state between test functions.
- **No ordering assumptions.** Tests must pass in any order, including alphabetic, reverse, and randomized.

## Discipline

Hard-won rules. Violating any of these is how a fast unit suite turns into a multi-minute CI hang.

### Extract pure validators from runners

Any orchestrator that performs I/O — spawns subprocesses, hits the network, calls a code-generation pipeline — MUST delegate argument validation to a pure helper. Tests of the argument logic call ONLY the helper; never the runner.

```
runX(args):
  validated, err = validateXArgs(args)   # pure: inputs → normalized values + error
  if err: return err
  ... slow I/O follows ...
```

Test `validateXArgs` from a unit test. Never `runX` — the runner is an integration- or e2e-tier concern.

### Isolate heavy tests behind a tag or marker

Anything that touches subprocesses, network, the filesystem outside a managed temp dir, time-based behavior, or external services belongs behind a tag/marker (Go build tags, pytest marks, Jest projects — whatever your runtime offers) so the default test command runs only fast unit tests with a tight timeout. Heavy tests opt-in.

### Determinism rule

Diagnostics, golden files, and assertions over hash-map data MUST sort keys before formatting or comparing. Map iteration order is non-deterministic in most languages; tests that compare formatted strings against a fixture will flake intermittently. Sort first, format second.

The same applies to any logged diagnostic output you intend to grep in tests.

<!-- @forge-only:start -->
## Library entry point: `pkg/tdd`

The canonical entry point for table-driven tests in a forge project is the `github.com/reliant-labs/forge/pkg/tdd` library. Forge's scaffolders (`forge new`, `forge add service`, `forge package new`, `forge generate`) emit unit / contract test files (plus CRUD integration tests when entities exist) that already import it. Scaffolded per-RPC rows are self-destructing — `WantErr: connect.CodeUnimplemented` fails the moment the handler is implemented, demanding a real assertion in its place; `pkg/tdd` has no permissive any-outcome mode. Treat the helpers below as the default vocabulary for new test files; reach for hand-rolled `for _, tc := range cases` only when the shape doesn't fit.

| Helper | Use |
|--------|-----|
| `tdd.Case[Req, Resp]` + `tdd.TableRPC` | unary Connect RPC tests (unit/integration/E2E) |
| `tdd.ContractCase` + `tdd.TableContract` | `internal/<pkg>/contract.go` Service tests |
| `tdd.E2EClient(t, srv, factory)` | `httptest.Server` → typed Connect client |
| `tdd.NewMock(opts...)` | terse Forge MockService construction |
| `tdd.AssertConnectError`, `tdd.WithTimeout`, `tdd.SetupMockDB` | standalone helpers |

See `testing/patterns` for copy-paste-ready templates.

## Forge test commands

```bash
forge test              # all test levels
forge test unit         # unit only
forge test integration  # integration only
forge test e2e          # e2e only
forge test --coverage   # with coverage report
forge test -V           # verbose; use when debugging failures
forge test --race       # Go race detector
```

## Go build tags (forge's tag/marker mechanism)

Forge enforces the "isolate heavy tests" discipline via Go build tags. Anything that touches subprocesses, network, filesystem outside `t.TempDir()`, time-based behavior, or external services MUST live in:

- `*_integration_test.go` with `//go:build integration` — DB-bound tests.
- `*_e2e_test.go` with `//go:build e2e` — full-stack flows.

Default `forge test` runs only the fast unit tests with a tight `-timeout 30s`.

## Canonical anti-pattern (real bug — kept here as a warning)

`TestRunNewKindValidation/empty_becomes_service` originally invoked the full `runNew` pipeline (network, filesystem, subprocesses) from a unit-test file. It hung tests for 99+ seconds and would have stalled CI indefinitely without an external timeout. Fix: extracted `validateNewArgs` (pure, in-memory) and rewrote the test to call only the helper. **If you find yourself writing `runX(cmd, args)` from a `_test.go` file in a unit context, stop and extract the validator first.**

## Sub-skills (forge)

- `testing/unit` — hermetic, fast handler-level tests.
- `testing/integration` — real-DB tests behind `//go:build integration`.
- `testing/e2e` — full-stack flows behind `//go:build e2e`.
- `testing/patterns` — copy-paste-ready table-driven templates for the four most common test shapes.

For Next.js / vite-spa / React frontends, this Go-flavored skill does NOT apply. Load the top-level `frontend-testing` skill instead — it covers Vitest + Testing Library patterns, the `mockTransport()` seam, and the four-state page coverage rule.
<!-- @forge-only:end -->
