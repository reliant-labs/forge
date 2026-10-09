---
name: observability
description: Local ClickStack and Pyroscope observability for Forge projects.
---

# Observability

Fresh Forge service projects enable local observability by default. The generated dev
KCL declaration projects application OTLP to the official, pinned ClickStack collector.
The local Compose stack runs ClickHouse, MongoDB, HyperDX, the official ClickStack
collector, Pyroscope, and profile-only Grafana Alloy.

Applications know only `OTEL_EXPORTER_OTLP_ENDPOINT`; they do not know ClickHouse,
HyperDX, MongoDB, or Pyroscope. Do not replace `clickhouse/clickstack-otel-collector`
with vanilla `otelcol-contrib`: its HyperDX exporter owns the supported ClickHouse
schemas. Alloy must collect only pprof profiles. It must not scrape Docker logs or
remote-write metrics, which would duplicate ClickStack signals.

The generated declaration is policy rather than a product-mode enum:

```kcl
_observability = {
    enabled = True
    endpoint = "auto" # or an explicit custom OTLP endpoint
    profiles = {provider = "pyroscope"}
}
```

`endpoint="auto"` starts the local stack and projects its loopback collector endpoint.
An explicit endpoint is the custom-OTLP escape hatch and starts no local backend.
Browser replay SDK adoption is intentionally deferred: this declaration is the safe
runtime configuration seam, but Forge does not claim replay works yet.

## Runtime validation

```bash
forge env status dev --signal traces
forge env status dev --signal metrics
forge env status dev --signal logs
forge env status dev --signal profiles
forge env status dev --json --verbose
```

Logs, traces, and metrics checks query ClickHouse tables written by the collector;
profiles checks Pyroscope. They report `UNDETERMINED` when a required endpoint cannot
be discovered and warn when the stack is healthy but no app signal arrived.

HyperDX is an operator UI with a dynamic Compose port. Its dashboard and alert APIs
are not a stable Forge contract, so phase 1 does not provision dashboards through an
unproven API. Existing backend-neutral Grafana dashboard definitions remain generated
inputs for a future Pyroscope/Grafana projection, not a ClickStack deployment feature.

## Pins and footprint

All Compose images use semantic tags plus immutable index digests. HyperDX and its
collector are `2.40.0`; ClickHouse is `26.1-alpine`; MongoDB is `8.0.14-noble`;
Pyroscope is `1.14.0`; Alloy is `v1.10.2`. The collector source is
`hyperdxio/hyperdx@0846f3b2a9d320f83c35fdb36651a67be477f8e4` under
`docker/otel-collector/` and `packages/otel-collector/` (MIT); verify its platform
manifest when changing architecture.

Expect roughly 2–3 GiB resident memory plus persistent ClickHouse, MongoDB, and
Pyroscope volumes in an idle local stack. MongoDB 5.0 from the upstream ClickStack
Compose is EOL and is not production-suitable; Forge uses MongoDB 8.0.14 locally.
A pinned HyperDX/Mongo compatibility test is required before either version changes.

## Upgrade

Projects containing legacy `_observability = True|False` retain a scaffold-once KCL
file. Replace that boolean with the declaration above, then run
`forge generate && forge env render dev`. Do not retain `lgtm` wiring: it points the
application at an obsolete backend. See `docs/proposals/observability.md` for why
profiles and Sentry remain parallel and why the official collector stays.
