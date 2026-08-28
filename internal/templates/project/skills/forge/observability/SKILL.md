---
name: observability
description: Observability — forge env status runtime checks, Grafana dashboards, querying logs/traces/metrics.
---

# Observability

Every Forge project ships with a full observability stack running locally via Docker Compose (Grafana LGTM: Grafana, Prometheus, Tempo, Loki, Pyroscope). No external services needed.

## forge env status — the runtime checks

Verify the entire observability pipeline is working:

```bash
forge env status dev                    # Table + every runtime check
forge env status dev --signal traces    # Check only traces
forge env status dev --signal metrics   # Check only Prometheus
forge env status dev --signal logs      # Check only Loki
forge env status dev --signal profiles  # Check only profiling
forge env status dev --signal app       # Check only /healthz + /readyz
forge env status dev --json             # Machine-readable output
forge env status dev --verbose          # Show evidence for passing checks
```

Checks: compose infra running, app health endpoint, pprof endpoint, Prometheus
targets up, Tempo traces ingested, Loki log streams present, Pyroscope profiles
available, Delve.

**These live on `forge env status`, not `forge doctor`.** They all need an
ADDRESS, and only `forge env status <env>` resolves one — it renders the same
KCL `forge env up` does and overlays the ports the live stack actually bound.
`forge doctor` had to guess (it assumed :8080), so on a project serving on
another port it printed a gray dash indistinguishable from "not applicable".
`forge doctor` now answers only "is this PROJECT well-formed" and takes no
`--env`.

A check that cannot obtain the facts it needs reports **UNDETERMINED** (`?`),
never a pass and never a skip. Three outcomes, not two.

Run it after `forge env up dev` to verify the pipeline is healthy before
investigating issues.

**The runtime checks verify the telemetry pipeline, NOT app-flow correctness.** They (like `forge env smoke`) are green when containers, endpoints, and signal ingestion are healthy — they can be green while the actual app flow is broken (e.g. a cross-cluster dial failing). To prove an app-flow invariant holds, use a declarative, exit-coded app-health assertion (model: a project `doctor:<flow>` task) plus a full `task test:e2e`. They tell you observability works; they do not certify the app does.

## Accessing Grafana

Grafana's port is dynamically assigned. Find it with:

```bash
docker compose ps          # Look for the lgtm container's port mapping
forge env status dev -v    # Evidence lines carry the discovered Grafana address
```

Two dashboards are auto-provisioned:
- **Logs Dashboard** — Log volume by level, filterable by service/container
- **Traces Dashboard** — Trace search, latency distribution, service map

## Querying Logs (Loki)

In Grafana → Explore → Loki, use LogQL:

```
{container=~".*app.*"} | json | level="error"
{container=~".*app.*"} | json | procedure="/services.users.v1.UsersService/Create"
{container=~".*app.*"} | json | trace_id="abc123"
```

Logs are structured JSON with consistent attribute keys (`procedure`, `request_id`, `trace_id`, `duration_ms`, `user_id`, `status`, `code`). Emit the same attribute keys from your own log sites so dashboards stay queryable.

## Querying Traces (Tempo)

In Grafana → Explore → Tempo, search by:
- Service name (matches your project name)
- Trace ID (from log lines — click a `trace_id` value to jump to the trace)
- Duration range
- Status code

Trace IDs are automatically injected into every log line, connecting logs to traces.

## Querying Metrics (Prometheus)

In Grafana → Explore → Prometheus, use PromQL. The RPC edge is measured by
the otelconnect server interceptor: `rpc_server_duration_milliseconds`
(histogram, labels `rpc_service`, `rpc_method`, and on errors
`rpc_connect_rpc_error_code`):

```
go_goroutines                                        # goroutine count per service
up                                                   # scrape targets status
rate(rpc_server_duration_milliseconds_count[5m])     # request rate
<pkg>_calls / <pkg>_errors / <pkg>_duration          # per-package in-process method metrics (component chain)
```

## In-process component observability (the middleware chain)

Forge instruments three boundaries, so a request is observable end to end:

1. **The RPC edge** — every Connect handler runs the interceptor chain built by
   `observe.Chain(observe.Deps{…})` in `cmd/<bin>/cmd/serve.go` (recovery →
   request-id → logging → tracing → metrics, then auth → audit → rate-limit,
   with otelconnect). One span / metric / log per RPC.
2. **The in-process component boundary** — every internal component→component
   method call (a `contract.go` `Service`) gets one span / metric / log plus
   panic-recovery. This is the layer detailed below — the in-process twin of the
   edge chain.
3. **The ORM** — `pkg/orm` registers the bun `bunotel` query hook, so every DB
   query becomes a child span.

The middle layer used to be dark. forge **no longer** scaffolds a hand-written
`observe.go` / `NewObserved` decorator that you extend by hand. Instead it
generates a slim per-method decorator — `middleware_gen.go` (forge-owned,
`// Code generated by forge. DO NOT EDIT.`, regenerated from the `Service`
interface on every `forge generate` and hash-tracked exactly like its sibling
`mock_gen.go`). Each generated wrapper method is a one-liner routing the inner
call through a `*observe.ComponentChain` (via `chain.Around` for the
`(T, error)` shape, `chain.Run` for the rest), so adding a method to the
interface regenerates the wrapper — there is nothing to maintain by hand. The
exported constructor is `New<Concrete>WithForgeMiddleware(inner Service)
Service`, named after the constructor's concrete return type: the canonical
`func New() Service { return &service{} }` yields `NewServiceWithForgeMiddleware`,
and the composition site emits `pkg.NewServiceWithForgeMiddleware(pkg.New(...))`
around your untouched `New`.

### The owned seam: `observe_chain.go`

You never touch the generated decorator. The chain it routes through is
assembled in an OWNED, scaffold-once file next to `contract.go`:

```go
// observe_chain.go — yours: scaffolded once, never overwritten by forge.
func newObserveChain() *observe.ComponentChain {
    scope := "<module>/internal/<pkg>"
    logger := slog.Default()
    return observe.NewComponentChain(
        observe.RecoverMiddleware(logger),                      // panic -> error, logged with stack
        observe.TraceMiddleware(otel.Tracer(scope)),            // one span "<pkg>.<Method>" per call
        observe.MetricsMiddleware(otel.Meter(scope), "<pkg>"),  // <pkg>.calls / .errors / .duration
        observe.LogMiddleware(logger, slog.LevelDebug),         // one structured record per call
    )
}
```

Middlewares run outer→inner in the order listed; each is nil-safe (no configured
tracer/meter degrades to pass-through), so a decorator wired in a test harness is
always safe. This file is THE extension point:

- **Add a layer** — implement `observe.ComponentMiddleware`
  (`WrapComponent(ctx, method, next) error`) and append it (an in-process
  timeout or rate-limit layer, say).
- **Drop a layer** — delete its line (a nil entry is dropped too).
- **Change the success-log level** — the `observe.LogMiddleware` argument.
  Failures always log at Error; successes log at this level. The scaffolded
  default is seeded from `observability.log_level` in forge.yaml (`debug` |
  `info` | `warn` | `error`; default `debug`, so success stays quiet under a
  production Info handler).

The chain captures only method identity, duration, and error status — never
arguments or results. It records `<pkg>.calls` / `<pkg>.errors` /
`<pkg>.duration` (each tagged `method="<pkg>.<Method>"`) and one span named
`<pkg>.<Method>` per call.

### Opting in and out

- **Opt in** — `// forge:constructor` on the `func New` doc comment. Scaffolds
  stamp it by default, so a new component is born instrumented. (Presence of the
  owned `observe_chain.go` seam also opts a package in, for backward
  compatibility.)
- **Opt a package out** — `// forge:no-observe` on the constructor (or the
  package / contract-interface doc). No decorator is generated and the
  composition site falls back to the unwrapped `pkg.New(...)`.
- **Opt ONE method out** — `// forge:no-observe` on that interface method's doc
  comment. The decorator still satisfies the interface, but that method
  delegates straight to the inner impl, around the chain.
- **Handler packages are never wrapped** — they return a concrete `*Service`
  and otelconnect already owns the RPC edge; wrapping would change the
  `Components` field type.

### The forcing function: `enforce-component-observe`

A wired component (a `Service` interface + a `New(Deps) Service` constructor)
that makes NO observability decision — neither marker — is flagged by the
`enforce-component-observe` lint: one aggregated ERROR naming every undecided
component with an I/O-aware suggestion (deps that touch a DB/adapter/client/HTTP
type are nudged toward `// forge:constructor`; a pure-compute component toward
`// forge:no-observe`). Kill-switch: `config.enforce_component_observe: off` in
forge.yaml (the sibling of `config.enforce_typed_access`).

For a one-off child span or metric at a single call site (rather than a whole
decorator), `observe.LogCall` / `observe.TraceCall` / `observe.NewCallMetrics`
remain available.

## Audit log (recipe)

Every RPC already produces a structured audit record: the scaffold wires
`fmw.AuditInterceptor(logger, middleware.ClaimsFromContext)` into the `Audit`
field of `observe.Chain(observe.Deps{…})` in the generated `cmd serve.go`. That
is slog-only — the record goes to your logs (queryable in Loki) with
`log_type=audit`, message `audit.event`.

For a **queryable, DB-persisted** audit trail (a compliance record you can page
through, plus an admin `ListAuditEvents` RPC), there is **no pack to install**.
The mechanical write-side is the versioned `forge/pkg/audit` library; the
app-specific read-side (the table + the RPC) is code you own.

1. **Own the `audit_log` table** with the normal entity flow — no bespoke
   migration path:

   ```bash
   # declare `// forge:entity message AuditEvent` in the service proto, then:
   forge scaffold
   ```

   Then edit the birth migration so the table matches what the library
   store reads/writes (see `audit.Entry`): `id`, `timestamp`, `user_id`,
   `email`, `procedure`, `peer_address`, `duration_ms`, `status`, `error_code`,
   `error_message`, `metadata` (JSONB), `created_at`. Index `user_id`,
   `procedure`, and `timestamp` for the query filters.

2. **Wire the DB-backed interceptor** from the library — one line. Construct
   the store and swap the base `Audit` field in `cmd serve.go`'s
   `observe.Deps`:

   ```go
   import "github.com/reliant-labs/forge/pkg/audit"

   store := audit.NewDBAuditStore(db)
   chainDeps := observe.Deps{
       // ...recovery/logging/tracing/metrics/auth/ratelimit as scaffolded...
       Audit: audit.Interceptor(logger, middleware.ClaimsFromContext, store),
   }
   ```

   `audit.Interceptor` logs to slog exactly as the base interceptor does AND
   persists each event to the store off the request path (fire-and-forget, so
   audit writes never add latency). A nil store falls back to slog-only. It is a
   thin convenience over `fmw.AuditInterceptorWithSink(logger, claimsFrom, sink)`
   — reach for that plus `audit.Sink(store, logger)` if you already hold a
   custom sink.

3. **Own the read-side `ListAuditEvents` RPC.** Add a proto service + handler
   the normal way (`proto/audit/v1/…` + a handler package), and back the
   handler with the SAME store so reads see the writes:

   ```go
   entries, err := store.Query(ctx, audit.Filter{
       UserID: req.Msg.GetUserId(),  // filters are AND-combined
       Since:  req.Msg.GetSince().AsTime(),
       Limit:  int(req.Msg.GetLimit()), // <= 0 ⇒ default 100
   })
   ```

   Audit logs enumerate who did what, so scope the read handler (restrict a
   non-admin caller to their own `user_id` via `middleware.ClaimsFromContext`)
   — an unscoped handler is a cross-user enumeration hole.

## Outbound instrumentation

- **Plain HTTP downstreams**: pass `infra.DefaultClient()` (providers.go) as
  the adapter's `Deps.HTTPClient` — a fresh client over the shared
  otelhttp-instrumented transport (client spans + W3C propagation + 30s
  timeout). The compose site wires any `HTTPClient *http.Client` Deps field
  to it automatically. If your providers.go predates `DefaultClient`, add the
  method + the `httpBase = otelhttp.NewTransport(http.DefaultTransport)`
  line from a fresh scaffold.
- **Connect clients** (a service split out of the binary): build the stack
  with `observe.NewClientStack` (forge/pkg/observe) — otelconnect client
  interceptor, request-ID forwarding, timeout — then
  `genconnect.NewXClient(stack.HTTPClient, baseURL, stack.ClientOptions...)`.

## Rules

- Run `forge env status dev` after `forge env up dev` to verify observability before investigating issues.
- Grafana port is dynamically assigned — use `docker compose ps` or `forge env status dev -v` to find it.
- The `alloy-config.alloy` and dashboard files are regenerated by `forge generate` — do not hand-edit.
- Use the `logevents.go` helpers for structured log events — do not create ad-hoc attribute keys.
- Trace IDs propagate automatically via OpenTelemetry context — no manual instrumentation needed.
