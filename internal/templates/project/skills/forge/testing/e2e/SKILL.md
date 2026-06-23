---
name: e2e
description: "End-to-end tests — full stack, multi-service flows, browser testing."
---

# End-to-End Tests

## Overview

E2E tests exercise the **full running stack** — real services, real database, real network calls between services. They live in the `e2e/` directory and validate multi-service flows as a user or client would experience them.

## Prerequisites

The full stack must be running before you execute e2e tests. Don't hand-roll the stack with `kubectl apply` or a bespoke compose file — `forge up --env=<env>` builds every service, brings up the compose-managed infra, and deploys each service to its declared target in one command:

```bash
forge up --env=dev     # in one terminal — builds + infra + deploys all services
forge test e2e         # in another terminal — runs e2e suite
```

If your services span multiple clusters, you don't script that yourself: each service's `deploy` block names its own `K8sCluster` (the `cluster` field is its kubectl context), and `forge up` routes each service to its context. A cross-cluster e2e flow is just real Connect clients talking to services that happen to live in different contexts.

## Running

```bash
forge test e2e                       # all e2e tests
forge test e2e --service <name>      # target a specific service's e2e tests
forge test -V                        # verbose output for debugging
```

## Multi-Service Flow Testing

E2E tests verify cross-service behavior over **real Connect RPC** calls. A typical test might:

1. Create a resource via Service A
2. Verify it propagates to Service B
3. Trigger a workflow and assert the final state

Use real Connect clients pointed at the running stack — mock nothing internal at this level. The only legitimate mock in an e2e test is a third-party boundary you don't own (a payment sandbox, an upstream provider); everything inside your system stays real.

**Auth at the e2e tier:** most flows can ride the dev-auth bypass (a synthetic dev token) to skip the login dance. But if the flow under test IS the auth path — login, token validation, role/tenant gating — turn the bypass off and drive a real token. An auth e2e test that runs under the bypass tests nothing.

## Determinism Requirements

E2E tests are the most flake-prone level. Follow these rules strictly:

- **No ordering assumptions** — tests must pass in any execution order
- **No fixed sleeps as a sync primitive** — `time.Sleep()` to "wait for it to settle" is always wrong. Poll the real state with a short interval (50–100ms) and a tight overall timeout (a few seconds), and fail with the last observed state. Once the infra is up, e2e tests should be *fast* — they wait for convergence, not for a wall-clock guess.
- **`t.Cleanup()` for everything** — every resource created must be cleaned up
- **Idempotent setup** — running the same test twice must produce the same result

## Debugging Failures

When an e2e test fails, the stack is still running. You can attach a debugger:

```bash
forge debug start
```

Inspect logs, database state, and service health while the failure is reproducible.

## Frontend E2E

For UI-level end-to-end tests:

- Use **chrome-devtools MCP** for browser interaction in agent-driven testing
- Use **Playwright** for traditional automated browser testing
- Same rules apply: deterministic, isolated, no production URLs

## Rules

- **Never hit production URLs** — all tests target the local stack only
- **Quarantine flaky tests** — mark with `t.Skip()` and link an issue reference
- **No test interdependence** — each test stands alone

## When to Drop Down a Level

If a scenario can be fully tested with mocked Connect clients and doesn't need the live stack, write it as an **integration test** instead. Reserve e2e for flows that genuinely require multiple running services.
