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
```

This keeps them out of `go test ./...` and ensures they only run when explicitly requested.

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
