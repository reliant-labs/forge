---
name: testing
description: "Testing methodology — mock vs real, harness patterns, flakiness prevention, and the test pyramid."
---

# Testing Methodology

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
