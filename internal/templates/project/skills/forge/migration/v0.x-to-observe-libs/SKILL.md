---
name: v0.x-to-observe-libs
description: Migrate from per-internal-package middleware/tracing/metrics codegen to forge/pkg/observe Connect interceptors + opt-in helpers. Mock stays codegen.
---

# Migrating from per-package wrapper codegen to `forge/pkg/observe`

Use this skill when `forge upgrade` reports a jump across the version that
ships `forge/pkg/observe` (typically `1.6.x → 1.7.x`). It supersedes the
`v0.x-to-contractkit` skill for the middleware/tracing/metrics rows; the
mock side is unchanged.

## 1. What changed

Forge versions before this release emitted four files per `internal/<pkg>/`
that defined `contract.go`:

- `mock_gen.go` — function-field mock
- `middleware_gen.go` — slog logging wrapper
- `tracing_gen.go` — OpenTelemetry tracing wrapper
- `metrics_gen.go` — OpenTelemetry metrics wrapper

The wrappers added per-method observability at the package boundary. In
practice they were almost never the right granularity: most observability
needs are request-scoped ("log this RPC", "trace this RPC"), not
method-scoped. Connect interceptors capture them at the handler boundary
once, with no codegen.

Forge 1.7+ keeps the **mock** (it's a real grep target — the per-method
`MockUserService.GetFunc` field is hard to replace with reflection) and
drops the other three. Observability moves into:

- `forge/pkg/observe.LoggingInterceptor(logger)`
- `forge/pkg/observe.TracingInterceptor(tracer)`
- `forge/pkg/observe.MetricsInterceptor(meter)`
- `forge/pkg/observe.RecoveryInterceptor(logger)`
- `forge/pkg/observe.RequestIDInterceptor()`

Plus a one-call canonical chain:

- `forge/pkg/observe.DefaultMiddlewares(deps DefaultMiddlewareDeps) []connect.Interceptor`

For the rare case where one Service calls another and you want a child
span / log line / metric, opt in per-call:

```go
user, err := observe.TraceCall(ctx, tracer, "userstore.Get", func(ctx context.Context) (User, error) {
    return s.userStore.Get(ctx, id)
})
```

## 2. Detection

How to tell which shape the project currently uses:

```bash
# Old shape: per-package wrappers exist.
find internal -name "middleware_gen.go" -o -name "tracing_gen.go" -o -name "metrics_gen.go" | head

# New shape: only mock_gen.go survives in internal/<pkg>/.
ls internal/*/mock_gen.go
```

## 3. Migration (deterministic part)

`forge generate` removes the stale wrappers automatically — the contract
generator now sweeps `middleware_gen.go`, `tracing_gen.go` and
`metrics_gen.go` from any directory it (re)generates a mock in.

```bash
# Apply: regenerate everything in-place.
forge generate

# Verify: no stale wrappers remain.
find internal -name "middleware_gen.go" -o -name "tracing_gen.go" -o -name "metrics_gen.go"

# Build should be clean. If it's not, see section 4 — there's almost
# certainly a hand-written reference into a now-removed symbol.
go build ./...
```

`pkg/app/bootstrap.go` is regenerated and no longer wraps internal
packages with `NewTracedService` / `NewMetricService`. The `pkgs.<X> =
…Impl` assignment is now direct.

`cmd/server.go` is regenerated and now wires the canonical chain via
`observe.DefaultMiddlewares(...)`. Project-specific interceptors (auth,
audit, rate-limit, otelconnect) are passed as `Extras`.

## 4. Migration (manual part)

What user code might need to change:

- **Direct references to `Instrumented<Iface>` / `Traced<Iface>` /
  `Metric<Iface>` types.** Search the project for these and replace
  with the bare interface type:

  ```bash
  grep -rn "Instrumented\|TracedService\|MetricService\|NewTracedService\|NewMetricService\|NewInstrumentedService" --include="*.go" .
  ```

  These names lived only in the now-removed wrappers; the mock side is
  untouched. If the project explicitly wired one of these (rare —
  `bootstrap.go` did this for you), drop the wrapper line and let the
  bare implementation flow through.

- **Inner-call observability.** If a service method previously got
  logging/tracing for free via the wrappers, you may now want
  `observe.TraceCall` / `observe.LogCall` / `observe.NewCallMetrics`
  at the inner call site. This is opt-in — the Connect interceptor
  layer covers the request-scoped case automatically.

- **Custom interceptor chain in cmd/server.go.** If you were already
  hand-managing `[]connect.Interceptor` in `cmd/server.go`, the
  regenerated file moves the canonical observability layer into
  `observe.DefaultMiddlewares(...)`. Extras you previously appended
  (auth, audit, rate-limit, otelconnect) flow through as
  `DefaultMiddlewareDeps.Extras`.

- **Code that imported `contractkit.LogCallErr` / `LogCall` /
  `TraceStart` / `RecordSpanError` / `NewMetrics`.** Those helpers are
  still present in `forge/pkg/contractkit` for backward compatibility,
  but new code should prefer `observe.LogCall` / `observe.TraceCall` /
  `observe.NewCallMetrics`. The contractkit equivalents continue to
  work; nothing forces an immediate edit.

## 5. Verification

```bash
go build ./... && go test ./... && forge lint
```

Plus a quick sanity check on the chain wiring:

```bash
grep "DefaultMiddlewares" cmd/server.go    # should be present
ls internal/*/{middleware,tracing,metrics}_gen.go 2>&1 | head    # should be empty
```

If all three pass, `forge upgrade` will bump `forge_version` in
`forge.yaml` to the target version automatically.

## 6. Rollback

If something breaks:

```bash
git revert <forge-generate-commit>      # undo the regen
forge upgrade --to 1.6.x                # pin back to the prior version
```

`--to 1.6.x` requires having the older forge build on `PATH` first;
install with `go install github.com/reliant-labs/forge/cmd/forge@vX.Y.Z`.

The `forge_version` field in `forge.yaml` will be reset to `1.6.x` so
subsequent `forge generate` runs won't warn about a mismatch with the
older binary.

## See also

- `observability` skill — the "right level for new instrumentation"
  question (interceptor vs helper vs handcoded).
- `auth` skill — `observe.DefaultMiddlewares` is where auth.Interceptor
  lands as an `Extra`.
- `contracts` skill — what `forge generate` produces today (just
  `mock_gen.go`).
