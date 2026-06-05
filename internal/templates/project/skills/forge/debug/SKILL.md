---
name: debug
description: Debug methodology — triage, parallel investigation, and synthesis.
emit: both
---

# Debug Methodology

## Triage First

Classify the bug before diving in:

- **Crash / panic** → check logs, attach a debugger
- **Wrong behavior** → trace code path, form hypotheses (see `investigate` sub-skill)
- **Only in multi-service flows** → reproduce with an e2e test (see `reproduce` sub-skill)
- **Flaky test** → run in a loop with the race detector enabled
- **Stale generated code** → regenerate first, then retest

## Parallel Investigation

Spawn agents to work simultaneously:

- **Researcher**: trace code paths, form hypotheses, check git log for recent changes
- **Tester**: write a failing test to isolate the bug via top-down bisection (see `isolate` sub-skill)
- **Reproducer**: add diagnostic logging, trigger in running system, collect evidence (see `reproduce` sub-skill)

## Synthesis

Combine findings from all tracks:

- **Root cause** with confidence level (High / Medium / Low)
- **Evidence** from each investigation track
- **Recommended fix** approach — hand off to an implementer, don't fix in debug mode

<!-- @forge-only:start -->
## Forge-Specific Debug Tools

```
forge run --debug              # attach Delve debugger on :2345
forge debug start <svc>        # debug a specific service
forge debug break              # set breakpoint in active debug session
forge debug continue           # resume execution past breakpoint
forge debug eval               # evaluate expression in debug context
forge test --service <name> -V # verbose isolated test runs
forge test e2e                 # full-stack reproduction
forge test --race              # run tests with race detector
forge generate                 # regenerate code (use when stale gen is suspected)
```

Use chrome-devtools MCP tools for frontend bugs (snapshots, console, network).

## Observability (Grafana LGTM)

`docker-compose` runs a Grafana LGTM stack with traces, metrics, logs, and continuous profiling.

- **Grafana UI:** http://localhost:3000 (no login needed — anonymous admin)
- **Traces:** Grafana → Explore → Tempo. Find slow requests, trace cross-service calls.
- **Metrics:** Grafana → Explore → Prometheus. Query `http_server_request_duration_seconds` etc.
- **Logs:** Grafana → Explore → Loki. Search structured logs.
- **Profiles:** Grafana → Explore → Pyroscope. CPU, heap, goroutine, mutex profiles from the app's pprof endpoint.

The app auto-connects: `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317` pushes traces and metrics. `PPROF_ADDR=localhost:6060` exposes pprof for Pyroscope scraping.

For LLM-driven observability, enable the Grafana MCP server from `.mcp.json.example` — it lets agents query Prometheus, Loki, Tempo, and dashboards directly.
<!-- @forge-only:end -->

## Sub-Skills

- `reproduce` — runtime evidence collection and e2e reproduction
- `isolate` — top-down bisection from e2e to unit test
- `investigate` — hypothesis formation and code tracing
