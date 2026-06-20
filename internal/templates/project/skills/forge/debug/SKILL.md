---
name: debug
description: Debug methodology — triage, parallel investigation, and synthesis.
emit: both
---

# Debug Methodology

## Triage First

Classify the bug before diving in:

- **Crash / panic** → check logs, attach a debugger.
- **Wrong behavior** → trace the code path, form hypotheses, then verify with a targeted test.
- **Only in multi-service / multi-process flows** → reproduce with an end-to-end test against the full stack.
- **Flaky test** → run in a loop with stress / race detection enabled.
- **Works locally, fails in CI** → diff the environment (versions, env vars, filesystem, time, network) before assuming a code bug.

## Parallel Investigation

If your environment supports parallel agents, split the work into three independent tracks:

- **Researcher** — trace code paths, form hypotheses, check git log for recent changes that touch the affected area.
- **Tester** — write a failing test that isolates the bug. Top-down bisection: start at the outermost reproducing layer (e2e), then narrow to the unit.
- **Reproducer** — add diagnostic logging, exercise the path in a running system, collect runtime evidence (request IDs, error chains, timing).

Working solo? Run the three passes sequentially: research → reproduce → isolate.

## Synthesis

Combine findings from all tracks before proposing a fix:

- **Root cause** with confidence level (High / Medium / Low).
- **Evidence** from each investigation track — which path, which test, which log line.
- **Recommended fix** approach — hand off to an implementer; don't fix in debug mode.

## Discipline

- **Reproduce before you guess.** A bug you can't trigger on demand is one you can't verify a fix for.
- **One hypothesis per test.** A failing test that "exercises the bug area" doesn't prove the hypothesis; one that fails for exactly one reason does.
- **Don't widen the diff.** Touch only what the evidence indicts; refactoring "while you're in there" turns a 1-line fix into a 200-line PR that needs its own review.

<!-- @forge-only:start -->
## Forge-Specific Triage

On top of the generic triage above, common forge-shaped bug classes:

- **Stale generated code** → run `forge generate` first, then retest. Forge generates from proto + forge.yaml; stale gen masquerades as a bug in hand-written code.
- **Broken DI wiring** → check the owned composition root `internal/app/build.go`. Deps are interface-typed fields resolved by type, so a missing fill is a compile error or a loud `validateDeps()` failure at construction, not a silent nil — but a wrong fill (an unintended optional dep left nil) can still surface as a nil-pointer panic deep in handler code. There is no generated `wire_gen.go` to inspect; the wiring is the hand-written `Build`.
- **Mock vs real divergence** → tests pass with generated mocks but the real adapter fails. Re-run the integration suite (`forge test integration`) before chasing a unit-test ghost.
- **Proto-DB drift** → entity types and DB schema evolve independently; `forge audit` flags the mismatch. If the symptom is "column X not found" in a handler that names the right struct field, audit first.

## Forge Debug Tools

```
forge debug start              # attach Delve debugger on :2345
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

## Sub-Skills (forge)

The three investigation tracks above each have a dedicated forge sub-skill with concrete patterns:

- `reproduce` — runtime evidence collection and e2e reproduction.
- `isolate` — top-down bisection from e2e to unit test.
- `investigate` — hypothesis formation and code tracing.

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
