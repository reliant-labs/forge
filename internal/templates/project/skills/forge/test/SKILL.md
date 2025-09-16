---
name: forge/test
description: Run unit and integration tests for Go services and frontends.
when_to_use:
  - You've changed handler code and need to verify tests pass
  - You want coverage numbers for a specific service
  - You need to reproduce a CI failure locally
---

# forge/test

`forge test` is the single entrypoint for unit and integration tests. Use it instead of raw `go test` so you get consistent flags and the right build tags — integration tests use `//go:build integration` and are invisible to plain `go test`.

## Core commands

```
forge test                      # unit + integration across all services and frontends
forge test unit                 # unit tests only (plus frontend tests)
forge test integration          # integration tests only (-tags integration)
forge test --coverage           # add -coverprofile=coverage.out
forge test --service <name>     # scope to one service under handlers/
forge test -V                   # verbose output (short for --test-verbose)
forge test --race=false         # disable race detector (default is on)
forge test --parallel=false     # run test suites sequentially
```

For end-to-end tests against a real cluster, see `forge/e2e-test`.

## Workflow

1. Establish a baseline before touching code:
   ```
   forge test --service <name>
   ```
2. Make your change. Re-run. If it fails, re-run with `-V` for verbose output.
3. If it's flaky, run the specific package directly:
   ```
   go test -count=10 -run TestName ./handlers/<svc>/...
   ```
4. Before pushing, run the full matrix:
   ```
   forge test
   ```

## Rules

- Unit tests live next to handlers as `*_test.go` with no build tag. Keep them hermetic — no network, no database.
- Integration tests use `//go:build integration` and may touch the dockerized postgres from `docker-compose.yml`.
- Don't `t.Skip()` a failing test. Fix the race or quarantine it explicitly with an issue reference.
- Never test generated code in `gen/`. If a generated type is wrong, fix the `.proto` and re-run `forge generate`.
- `coverage.out` is not cleaned between runs. The `.gitignore` ignores `*.out` so it won't end up in commits, but stale coverage data can confuse tooling — `rm coverage.out` before a fresh coverage run if you're comparing numbers.

## When this skill is not enough

- End-to-end scenarios with a real cluster → `forge/e2e-test`.
- Benchmarks → `go test -bench=. -benchmem ./handlers/<svc>/...` directly.
- Fuzzing → `go test -fuzz=FuzzName -fuzztime=30s ./handlers/<svc>/...` directly.
