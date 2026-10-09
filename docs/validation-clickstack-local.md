# Local ClickStack validation

Phase 1 validates the generated Compose contract in unit and scaffold-render tests.
To run the optional Docker integration manually from a fresh generated service project:

```bash
forge env up dev
forge env status dev --signal traces --verbose
forge env status dev --signal metrics --verbose
forge env status dev --signal logs --verbose
forge env status dev --signal profiles --verbose
```

Make one application request before the ClickStack checks: OTLP batching means an idle
service has no trace/log/metric rows and status correctly reports a warning rather than
a pass. The checks query `otel_traces`, `otel_metrics_sum`, and `otel_logs` in the
ClickHouse instance fed by the official ClickStack collector. Profiles are checked
separately in Pyroscope.

A CI Docker ingestion test is intentionally not enabled in this phase: it must prove
HyperDX 2.40.0 plus MongoDB 8.0.14 compatibility and inspect the exact collector schema
on both supported CPU architectures. Add that test before changing either image pin;
its fixture must emit a log, trace, and metric through `otel-collector:4317`, then prove
the rows are visible in ClickHouse and the HyperDX UI process is healthy. The current
collector image provenance is `hyperdxio/hyperdx@0846f3b2a9d320f83c35fdb36651a67be477f8e4`
(`docker/otel-collector/`, `packages/otel-collector/`); its component BOM is a release
follow-up, not an assumed vanilla contrib collector.
