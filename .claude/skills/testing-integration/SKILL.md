---
name: integration
description: "Integration tests — real database, build tags, transaction isolation."
---

# Integration Tests

## Overview

Integration tests run against a **real database** and verify that queries, migrations, and data access layers work correctly. They are invisible to plain `go test` because they require a build tag.

## Build Tag

Every integration test file must include the build constraint at the top:

```go
//go:build integration

package handlers_test
```

This keeps them out of `go test ./...` and ensures they only run when explicitly requested.

## Convention: build tag, never `testing.Short()`

Forge integration tests use the **`//go:build integration`** build tag — *not* `testing.Short()` with `t.Skip(...)`. The two mechanisms answer different questions:

| Mechanism | What it does | Use when |
| --- | --- | --- |
| `//go:build integration` | The Go toolchain physically excludes the file from `go build` / `go test` unless `-tags integration` is passed. No compile, no link, no skip-at-runtime. | The test needs infra that may not be present (DB, broker, network). The default test run must not even attempt it. |
| `if testing.Short() { t.Skip(...) }` | The test is always compiled and runs by default. `go test -short` skips it at runtime. | The test is hermetic but optionally slow — e.g. a fast unit test that has an extra long-running scenario behind `-short`. |

**Rule for forge projects:** all DB-backed / RPC-roundtrip / infra-dependent tests use the build tag. Reserve `testing.Short()` for the rare case of a hermetic test with a fast/slow toggle. If your test would `t.Skip` because it can't reach a database, it belongs behind `//go:build integration` instead.

The matching convention for end-to-end tests is `//go:build e2e`.

## Running

```bash
forge test integration              # runs all integration tests
forge test integration --service <name>  # one service only
forge test --coverage               # includes integration in coverage
```

The `forge test integration` command automatically adds `-tags integration` to the Go test invocation.

## Prerequisites

Integration tests require a running Postgres instance via docker-compose. If you're already running the stack, you're set:

```bash
forge run    # starts the full stack including postgres
```

## Transaction-Per-Test Isolation

Each test runs inside a database transaction that is rolled back in cleanup. This guarantees complete state isolation between tests:

```go
func TestCreateUser(t *testing.T) {
    tx := testdb.Begin(t)       // starts transaction
    t.Cleanup(func() { tx.Rollback() })
    queries := db.New(tx)
    // ... test with real queries
}
```

## Testing sqlc Queries

Integration tests are the right place to verify sqlc-generated queries against the real schema. Test the actual SQL — don't mock the database when the query behavior is what you're validating.

## Seed Data

- Create test data in the test function or a helper called from the test
- Always clean up with `t.Cleanup()` (transaction rollback handles this automatically)
- **Never rely on test ordering** — each test must set up its own state

## Rules

- **No mocking the DB** — if you're testing queries, use the real database
- **Deterministic assertions** — don't assert on auto-generated IDs or timestamps without pinning them
- **Don't share state** — no package-level variables mutated across tests
- If a test doesn't need the database, it belongs at the unit level
