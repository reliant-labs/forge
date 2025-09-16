---
name: forge/e2e-test
description: Run end-to-end tests against a running stack.
when_to_use:
  - You need to verify multi-service flows that unit tests cannot cover
  - A bug only reproduces when services talk to each other over Connect RPC
  - You're validating a deployment change
---

# forge/e2e-test

End-to-end tests live under `e2e/` and run with `go test -v ./e2e/...` via `forge test e2e`. The test command does **not** boot the stack for you — you must start it yourself first.

## Core commands

```
forge test e2e                      # run all e2e tests (-v is always set)
forge test e2e --service <name>     # scope to e2e/<service>/... only
```

## Workflow

1. Boot the stack in another terminal:
   ```
   forge run
   ```
   Wait for services to report healthy.
2. Run the e2e suite:
   ```
   forge test e2e
   ```
3. Iterate on a single test much faster with plain go test:
   ```
   go test -v ./e2e/... -run TestUserSignupFlow
   ```
4. When a test fails, the stack is still running. Attach a debugger:
   ```
   forge debug start <failing-service>
   ```
   Or use the `chrome-devtools` MCP to drive the frontend that triggered the failure.

## Rules

- E2E tests must be deterministic. No ordering assumptions between tests, no real-time sleeps, no brittle timeouts.
- Each test must clean up after itself via `t.Cleanup()`. Otherwise the suite becomes order-dependent.
- Never hit production URLs from e2e tests. The harness is pointed at local k3d / docker-compose only.
- If an e2e test is flaky, quarantine it and open an issue. Retry loops hide real race conditions.
- `forge test e2e` returns a clear error if `e2e/` doesn't exist — it does not silently pass.

## When this skill is not enough

- The scenario can be tested with mocked Connect clients → drop back to `forge/test integration`.
- You need to reproduce a production bug → build a minimal e2e test that reproduces it, then fix the code, then keep the test as a regression.
