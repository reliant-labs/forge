---
name: observability
description: Observability — the local ClickStack (HyperDX) that forge env up runs by default, forge env status runtime checks, querying logs/traces/metrics, dashboards.
---

# Observability

Every Forge project ships a full observability stack for the dev loop: **ClickStack** (ClickHouse, the HyperDX UI and an OTLP collector) as the `clickstack` service in `docker-compose.yml`. Nothing to register, no key to copy. It is **on by default**: `forge env up dev` starts it, and the summary lists the **HyperDX UI** under **Compose services**.

It is one container (HyperDX's own no-auth *local mode* image), about 0.8 GB resident. Every port is published on `127.0.0.1` only; ClickHouse is not published at all and runs behind a generated dev-only user rather than the passwordless `default`.

## How the data gets there

| Signal | Path |
|---|---|
| Traces, metrics | Host processes started by `forge env up` get `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:<port>` and `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` and push OTLP/HTTP to the collector. Compose services use `http://otel-collector:4318`. The app never learns the backend: it reads the standard variables and nothing else. |
| Logs | The collector tails `.forge/logs/<env>/<service>.log` — the files `forge env up` already tees every host process into — parsing the JSON lines the runtime writes. `service.name` is the file name (the workload name), `trace_id` / `span_id` become the log's trace link. This is the **one** local log path; the runtime does not also export logs over OTLP, so nothing is duplicated. The pipeline is `deploy/observability/otel-collector.yaml` — yours to edit. |

A value you set yourself (shell, `config.k`, a workload's `env`) wins: `forge env up` fills `OTEL_EXPORTER_OTLP_ENDPOINT` / `_PROTOCOL` only when unset or empty.

**To turn it off**, set `_observability = False` in `deploy/kcl/dev/main.k`: no container starts and no endpoint is exported. Trace context still propagates. Profiles are not collected locally yet.

## forge env status — the runtime checks

Verify the entire observability pipeline is working:

```bash
forge env status dev                    # Table + every runtime check
forge env status dev --signal traces    # Check only traces
forge env status dev --signal metrics   # Check only metrics
forge env status dev --signal logs      # Check only logs
forge env status dev --signal profiles  # Check only pprof
forge env status dev --signal app       # Check only /healthz + /readyz
forge env status dev --json             # Machine-readable output
forge env status dev --verbose          # Show evidence for passing checks
```

Checks: compose infra, app health, pprof (and that the process answering it IS
the app), traces, metrics (gauge, sum and histogram) and logs from the last 15
minutes in ClickHouse, each naming the services that sent them, and Delve.

They read ClickHouse itself (`docker compose exec` into `clickstack`, with its
own credentials), so a pass means rows are in the store, not that a UI answers.
With ClickStack off they SKIP and say so.

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

## Accessing HyperDX

`forge env up dev` and `forge env status dev` list the **HyperDX UI** URL under **Compose services** (its port is declared once in `deploy/kcl/dev/main.k`, `_hyperdx_port`, default 8180). There is no sign-in. Three sources are pre-created — **Logs**, **Traces**, **Metrics** — so search works on first open.

### Dashboards

HyperDX dashboard JSON dropped into `deploy/observability/dashboards/` is loaded automatically, within about 15 seconds and without a restart (HyperDX's file provisioner, `DASHBOARD_PROVISIONER_DIR`). A provisioned dashboard is read-only in the UI and replaced by name on the next load: edit the file, not the browser copy. A dashboard you build by hand in the UI is separate and never overwritten. The directory is yours; forge scaffolds it empty.

## Querying Logs

In HyperDX → Search → Logs, filter on the service (the workload name), severity, or any attribute the log line carried:

```
ServiceName:api SeverityText:error
ServiceName:api LogAttributes.procedure:"/services.users.v1.UsersService/Create"
TraceId:abc123
```

Logs are structured JSON with consistent attribute keys (`procedure`, `request_id`, `trace_id`, `duration_ms`, `user_id`, `status`, `code`, `error_class`). Emit the same attribute keys from your own log sites so dashboards stay queryable. A line that is not JSON (air's rebuild output, a panic) is kept as plain text under the same service.

Or ask ClickHouse directly:

```bash
docker compose exec clickstack sh -c 'clickhouse-client -u "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" \
  -q "SELECT Timestamp, ServiceName, SeverityText, Body FROM default.otel_logs ORDER BY Timestamp DESC LIMIT 20"'
```

## Error levels: who has to act

A log level is a routing decision: ERROR is what alerts match, what a Sentry
forwarder sends, what a post-deploy log gate fails on. So forge sets the level
of every error record from **fault attribution** — `svcerr.Classify(err)` —
not from how the call site spelled its log line:

| `error_class` | what it means | example | level | pages (ERROR → Sentry / alerts) |
|---|---|---|---|---|
| `server` | our system failed, or cannot tell that it did not | Internal, Unknown, DataLoss, Unavailable, any unrecognised error | ERROR | yes |
| `server` | server-side, but "retry", not "page" | DeadlineExceeded, Aborted, Unimplemented | WARN | no |
| `user` | the caller or user must act; the system behaved correctly | InvalidArgument, NotFound, AlreadyExists, PermissionDenied, Unauthenticated, FailedPrecondition (InsufficientBalance, Expired), OutOfRange, ResourceExhausted (PlanLimit), or `svcerr.WithClass(err, svcerr.ClassUser)` | INFO (`observe.WithUserErrorLevel`) | no |
| `canceled` | the caller went away | `context.Canceled`, CodeCanceled | same as `user` | no |

Nothing is hidden: a user error is still written, with `error`,
`error_class=user` and `code`, at a level production keeps — it just stops
claiming to be an incident. Its volume stays countable: the RPC error counter
(and `<pkg>.errors` on the component chain) carries `error_class` and `code`.

The policy holds at all three places a record comes from:

1. `rpc failed` / `stream failed` at the RPC edge (`observe.LoggingInterceptor`);
2. component failures on the in-process chain (`observe.LogMiddleware`);
3. **every hand-written `logger.Error`** — `observe.NewErrorClassHandler`
   wraps the process's slog handler (serverkit's `NewLogger` already does) and
   lowers a record carrying a non-server error to the user-error level, adding
   `error_class`. It only ever lowers; it never raises a WARN to ERROR.

**If you forward logs to Sentry (or count them), wrap
`observe.NewErrorClassHandler` OUTSIDE that handler**, so it sees the
classified level — the forwarder keeps gating on `level >= ERROR` and never
needs to know about svcerr:

```go
handler := sentryHandler(serverkit.NewLogger(cfg).Handler()) // forwards >= ERROR
logger := slog.New(observe.NewErrorClassHandler(handler))
```

Marking: most user errors need no marker — return the right `svcerr` kind
(`svcerr.FailedPrecondition`, `svcerr.PlanLimit`, …). Use
`svcerr.WithClass(err, svcerr.ClassUser)` when the CODE and the FAULT disagree
(`Unavailable` is the honest code for "the user's machine is offline", and a
server fault by kind), or when an error never crosses an RPC boundary (a worker
logging a provider's "credential rejected"). `svcerr.WithClass(err,
svcerr.ClassServer)` is the reverse: a 4xx the client must see that is really
our bug. A library error svcerr cannot see into — a workflow engine's
application error that crossed a serialization boundary — is taught with
`observe.WithErrorClassifier(func(error) svcerr.Class)` (return
`svcerr.ClassNone` for "no opinion").

## Querying Traces

In HyperDX → Search → Traces, filter by:
- Service name (the workload name)
- Trace ID (from log lines — a log with a `TraceId` links to its trace)
- Duration range
- Status code

Trace IDs are automatically injected into every log line, connecting logs to traces.

## Querying Metrics

In HyperDX → Search → Metrics (or a chart tile), pick the metric by name. The RPC edge is measured by the otelconnect server interceptor: `rpc.server.call.duration` (histogram, attributes `rpc.method` — the full `pkg.Service/Method` — and `rpc.response.status_code`, `OK` or the upper-case Connect code such as `NOT_FOUND`). Other series the app exports: `db.sql.*` / `go.sql.*` for the DB pool and queries, and `<pkg>_calls` / `<pkg>_errors` / `<pkg>_duration` for the per-package in-process method metrics (component chain). Every series carries `ServiceName`.

## In-process component observability (the middleware chain)

Forge instruments three boundaries, so a request is observable end to end:

1. **The RPC edge** — every Connect handler runs the interceptor chain built by
   `observe.Chain(observe.Deps{…})` in `cmd/<bin>/cmd/serve.go` (recovery →
   request-id → logging → tracing → metrics, then auth → audit → rate-limit,
   with otelconnect). One span, one metric sample and one log record per RPC:
   `rpc failed` for every failure, `rpc completed` for every success, and
   `slow=true` on any success over 1s. Success logging can be SAMPLED — see
   [Success-log sampling](#success-log-sampling) below. Tune the layer
   through `observe.Deps.LogOptions` —
   `observe.WithSuccessLevel(<svc>connect.<Svc><Method>Procedure, slog.LevelDebug)`
   silences one poll, `observe.WithSuccessSampling(d)` fixes this layer's
   sampling window, `observe.WithSlowThreshold(d)` moves the slow line.
2. **The in-process component boundary** — every internal component→component
   method call (a `contract.go` `Service`) gets one span, one metric sample
   and one log record (every failure, every success unless sampling is on),
   plus panic-recovery. This is the layer detailed below — the in-process
   twin of the edge chain.
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
        observe.LogMiddleware(logger, slog.LevelDebug),         // every failure; every success (or a sample)
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
  Failures log at their class's level (ERROR for a server fault, INFO for a
  user error or cancellation — see
  [Error levels](#error-levels-who-has-to-act)); successes log at this
  level. The scaffolded
  default is seeded from `observability.log_level` in forge.yaml (`debug` |
  `info` | `warn` | `error`; default `debug`, so success stays quiet under a
  production Info handler).
- **Tune success logging** — every success is logged (see
  [Success-log sampling](#success-log-sampling)); a call over 1s is always
  logged with `slow=true`. Trailing options on `observe.LogMiddleware` change
  that for this package:
  `observe.WithSuccessLevel("<pkg>.<Method>", slog.LevelInfo)` for one method,
  `observe.WithSuccessSampling(d)` to sample this package's successes
  (`0` = every success),
  `observe.WithSlowThreshold(d)` to move the slow line.
- **Declare expected errors** — an error that is an ANSWER, not a failure
  (a storage read for a missing object behind a 404, a registry asked for a
  repository it does not hold), is declared on `observe.LogMiddleware`:
  `observe.WithExpectedErrors(ErrNotFound)` (`errors.Is` against each
  target), or `observe.WithExpectedErrorFunc(func(err error) bool)` for a
  classification no sentinel expresses. A declared error is logged like a
  success — at the success level, sampled in its own slot, carrying the
  error and `expected=true` — so it is quiet under a production INFO handler
  while every undeclared error keeps its class's level. Nothing is expected
  until declared. The same options work at the RPC edge through
  `observe.Deps.LogOptions` (a declared NotFound logs `rpc failed` at the
  procedure's success level, sampled with its successes). Logging only: the
  span and `<pkg>.errors` still record the error.
- **Change the user-error level** — `observe.WithUserErrorLevel(slog.LevelWarn)`
  on `observe.LogMiddleware` (or in `observe.Deps.LogOptions` for the RPC
  edge) for a deployment that wants user errors in a WARN view.

The chain captures only method identity, duration, and error status — never
arguments or results. It records `<pkg>.calls` / `<pkg>.errors` /
`<pkg>.duration` (each tagged `method="<pkg>.<Method>"`) and one span named
`<pkg>.<Method>` per call.

### Success-log sampling

Every successful call is logged by default — at the RPC edge and at every
component boundary. That is what you want in dev, and in any process whose
traffic you can afford to read. Where an access log's volume (which is the
traffic's volume) outgrows its value — a frontend polling three RPCs every
couple of seconds wrote 47k `rpc completed` lines in twelve hours of one dev
stack — turn on sampling for that deployment:

```kcl
# deploy/kcl/<env>/config.k
app_config: config_gen.AppConfig = {
    log_success_sample_window = "1m"
}
```

`log_success_sample_window` is declared in `proto/config/v1/config.proto`,
so the config loader types and validates it, and it can come from any source
the loader reads (`--log-success-sample-window`, `LOG_SUCCESS_SAMPLE_WINDOW`,
a `--config` file). The scaffolded `serve.go` passes the loaded value to the
RPC edge's logging interceptor:

```go
chainDeps := observe.Deps{
    // ...
    LogOptions: []observe.LogOption{observe.WithSuccessSampling(cfg.LogSuccessSampleWindow.AsDuration())},
}
```

forge's `observe` package reads no environment — the window is only ever
the one its caller passed. So an older project adopts it by adding the field
to its AppConfig and that one `LogOptions` line to its owned `serve.go`; a
server that is not forge-scaffolded passes `observe.WithSuccessSampling` in
`DefaultMiddlewareDeps.LogOptions` from its own config.

Sampled, each RPC procedure logs its first success, then at most one per
window carrying `suppressed=<n>` — the successes that record stands for, so
the rate survives in the log. Volume is bounded by the number of procedures,
not by traffic. Never sampled, at any setting: failures (every one, with its
error) and successes slower than 1s (`slow=true`).

Component-call logs (`observe_chain.go`) default to DEBUG, so a production
INFO handler already drops their successes. To sample one package's anyway,
append `observe.WithSuccessSampling(d)` to its `observe.LogMiddleware`.

`0` (or a negative window) logs every success. When a layer is given more
than one `WithSuccessSampling`, the last one wins.

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
is slog-only — the record goes to your logs (queryable in HyperDX) with
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
- The HyperDX port is declared in `deploy/kcl/dev/main.k` and listed by `forge env up dev` / `forge env status dev`.
- `deploy/observability/otel-collector.yaml` and `deploy/observability/dashboards/` are yours: forge scaffolds them once and never rewrites them.
- Use the `logevents.go` helpers for structured log events — do not create ad-hoc attribute keys.
- Trace IDs propagate automatically via OpenTelemetry context — no manual instrumentation needed.
