---
name: observability/tracing-lint
description: forge lint --tracing — the four rules that keep a request in one trace (log-without-ctx with its autofix, raw-http-client, raw-sql-open, contract-uninstrumented), lint.tracing.log_wrappers for your own logging helpers, and adopting the rules in a large codebase.
applies: any
---

# Observability: keeping a request in one trace (`forge lint --tracing`)

A trace fragments wherever app code steps around the traced constructors.
`forge lint` gates on four such places, and `forge lint --tracing` runs them
alone:

| Rule | Flags | Fix |
|---|---|---|
| `forge-tracing-log-without-ctx` | `slog.Info(…)` / `logger.Info(…)` where a `ctx` (or `r *http.Request`) is in scope: the line has no trace_id | autofixed to `slog.InfoContext(ctx, …)` |
| `forge-tracing-raw-http-client` | `&http.Client{}` with no/nil/default transport, `http.DefaultClient`, `http.Get` | `observe.NewHTTPClient(observe.HTTPClientOptions{…})` |
| `forge-tracing-raw-sql-open` | `sql.Open`, `sqlx.Connect`, a pgx pool with no tracer | `dbtrace.Open(driver, dsn)`; for a native pgx pool, `cfg.ConnConfig.Tracer = dbtrace.NewPgxTracer()` before `pgxpool.NewWithConfig` |
| `forge-tracing-contract-uninstrumented` | an instrumented contract's `pkg.New(…)` used without its decorator, anywhere (`providers.go`, `*_wire.go`, another binary) | autofixed to `pkg.NewInstrumented(…)` — New's signature, decorated |

The rules cover only code a traced process can run, so two places are left
alone without a directive:

- **Programs no traced process runs.** A `package main` is traced when its
  local import closure imports `forge/pkg/serverkit` or any
  `go.opentelemetry.io/` package, or calls `observe.Setup`. A main that does
  none of that (a load generator, an example client) installs no tracer, so
  it and the packages only such mains reach are skipped, unless another
  module can run them through a public package. A package no main reaches
  is still linted. Build-constraint uncertainty keeps code linted too: if any
  file in a main's closure is excluded by the current Go build context, that
  main cannot establish the untraced exemption.
- **A library's default for a client its caller supplies.** In a package
  another module can import directly (not its internal implementation dependencies),
  `if o.HTTPClient == nil { o.HTTPClient = &http.Client{} }` (or
  `http.DefaultClient`, assigned to the very value tested) is not flagged:
  tracing is the caller's choice. Internal code keeps the finding. A public
  SDK file that must not import forge but builds clients some other way
  takes `// forge:lint-disable-file forge-tracing-raw-http-client: <why>`.

A logging helper of your own (`logging.Info(msg, args...)`) joins the log rule
through forge.yaml:

```yaml
lint:
  tracing:
    log_wrappers:
      - call: github.com/acme/app/internal/logging.Info
        context_call: log/slog.InfoContext   # default: logging.InfoContext
```

Adopting the rules in a big codebase: set them to `warn` under `lint.rules`,
run `forge lint --tracing --fix` once (it rewrites every log call and every
undecorated contract construction at any severity but off; `forge lint --fix`
rewrites only at error, and without `--fix` nothing is rewritten), fix the
rest, then drop the
`warn` entries.
