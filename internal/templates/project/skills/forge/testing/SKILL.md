---
name: testing
description: "Testing methodology — mock vs real, harness patterns, flakiness prevention, and the test pyramid."
---

# Testing Methodology

## Library entry point: `pkg/tdd`

The canonical entry point for table-driven tests is the
`github.com/reliant-labs/forge/pkg/tdd` library. Forge's scaffolders
(`forge new`, `forge add service`, `forge package new`, `forge generate`)
emit unit / integration / E2E / contract test files that already import
it. Treat the helpers below as the default vocabulary for new test
files; reach for hand-rolled `for _, tc := range cases` only when the
shape doesn't fit.

| Helper | Use |
|--------|-----|
| `tdd.Case[Req, Resp]` + `tdd.TableRPC` | unary Connect RPC tests (unit/integration/E2E) |
| `tdd.ContractCase` + `tdd.TableContract` | `internal/<pkg>/contract.go` Service tests |
| `tdd.E2EClient(t, srv, factory)` | httptest.Server → typed Connect client |
| `tdd.NewMock(opts...)` | terse Forge MockService construction |
| `tdd.AssertConnectError`, `tdd.WithTimeout`, `tdd.SetupMockDB` | standalone helpers |

See `testing/patterns` for copy-paste-ready templates.

## The Test Pyramid

Structure tests in layers, with volume decreasing as scope increases:

1. **Unit** — fast, hermetic, handler-level (see `testing/unit`)
2. **Integration** — real database, build-tagged (see `testing/integration`)
3. **E2E** — full stack, multi-service flows (see `testing/e2e`)

Run all levels at once or individually:

```bash
forge test              # all test levels
forge test unit         # unit only
forge test integration  # integration only
forge test e2e          # e2e only
forge test --coverage   # with coverage report
```

## Mock vs Real Decision Framework

This is the most critical testing decision. Get it wrong and tests either break constantly or miss real bugs.

### Keep REAL
- **The thing under test** — never mock what you're testing
- **Fast, deterministic things** — pure functions, in-memory data structures, value objects
- **Things where mocking hides bugs** — ORM behavior, SQL queries, serialization logic

### MOCK
- **External services** — third-party APIs, email providers, payment gateways
- **Non-deterministic sources** — time, random numbers, network calls
- **Slow irrelevant resources** — services not related to the behavior under test

### The Boundary Rule
Mock at **system boundaries**, keep internals real. If two components live in the same service, test them together. If they cross a network boundary, mock the far side.

## Test Harness Patterns

- **Test database**: use transaction-per-test with rollback for isolation
- **External services**: mock at the client interface using generated mocks
- **Authenticated test client**: helper that returns a pre-authenticated Connect client
- **Fixtures**: deterministic seed data, created in test setup, cleaned in `t.Cleanup()`

## Flakiness Prevention

Flaky tests erode trust. Follow these rules strictly:

- **Never use fixed sleeps** — `time.Sleep(2 * time.Second)` is always wrong
- **Poll with timeout** for async operations — retry loop with deadline
- **Event-driven waiting** — use channels, condition variables, or test hooks
- **Deterministic seeds** — if randomness is involved, seed it and log the seed
- **Isolate state per test** — no shared mutable state between test functions
- **No ordering assumptions** — tests must pass in any order

## Verbose Output

When debugging test failures, use verbose mode:

```bash
forge test -V
```

## Discipline

Hard-won rules. Violating any of these is how unit tests turn into 99-second CI hangs.

### Extract pure validators from runners

Cobra `runX(cmd, args) error` (and any other orchestrator that performs I/O, spawns subprocesses, or calls into a generator pipeline) MUST delegate validation to a pure helper — `validateXArgs(...) (..., error)`. Tests of the runner's argument logic call ONLY the helper, never the runner itself.

```go
// runNew touches the filesystem, runs `go mod tidy`, calls `buf generate`.
// validateNewArgs is pure: takes flag values, returns normalized values + error.
func validateNewArgs(kindFlag, bufPlugins string, services, frontends []string) (kind, plugins string, err error) { ... }
func runNew(...) error { kind, plugins, err := validateNewArgs(...); /* ... slow I/O ... */ }
```

Test the validator. Never the runner from a unit test.

### Build-tag heavy tests

Anything that touches subprocesses, network, the filesystem outside `t.TempDir()`, time-based behavior, or external services MUST live in a `*_e2e_test.go` file with `//go:build e2e` (or `integration` for DB-bound tests). Default `task test` runs only fast unit tests with a tight `-timeout 30s`.

### Canonical anti-pattern

`TestRunNewKindValidation/empty_becomes_service` originally invoked the full `runNew` pipeline (network, filesystem, subprocesses) from a unit-test file. It hung tests for 99+ seconds and would have stalled CI indefinitely without an external timeout. Fix: extracted `validateNewArgs` (pure, in-memory) and rewrote the test to call only the helper. **If you find yourself writing `runX(cmd, args)` from a `_test.go` file in a unit context, stop and extract the validator first.**

### Determinism rule

Diagnostics, golden files, and assertions over map data MUST sort keys before formatting or comparing. Map iteration order is non-deterministic; tests that compare formatted strings against a fixture will flake intermittently.

```go
keys := make([]string, 0, len(m))
for k := range m { keys = append(keys, k) }
sort.Strings(keys)
for _, k := range keys { fmt.Fprintf(&buf, "%s=%s\n", k, m[k]) }
```

Same applies to logging diagnostic output you intend to grep in tests.

## Sub-skills

- `testing/unit` — hermetic, fast handler-level tests
- `testing/integration` — real-DB tests behind `//go:build integration`
- `testing/e2e` — full-stack flows behind `//go:build e2e`
- `testing/patterns` — copy-paste-ready table-driven templates for the four most common test shapes
