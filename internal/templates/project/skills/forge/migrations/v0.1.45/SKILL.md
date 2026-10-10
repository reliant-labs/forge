---
name: v0.1.45
description: Move local observability from the Grafana LGTM stack (and the opt-in Alloy collector) to ClickStack, which is now ON by default in `forge env up`. Use when docker-compose.yml still has an `lgtm` or `alloy` service, deploy/kcl/dev/main.k still declares an `lgtm` workload, or deploy/alloy-config.alloy exists.
version: v0.1.45
detection: test -f deploy/alloy-config.alloy || grep -qsE 'grafana/otel-lgtm|name = "lgtm"' docker-compose.yml deploy/kcl/dev/main.k
---

# Local observability: LGTM becomes ClickStack

`forge env up` now runs ClickStack (ClickHouse, the HyperDX UI and an OTLP
collector in one container) instead of Grafana LGTM, and it is on by default.
Host processes get `OTEL_EXPORTER_OTLP_ENDPOINT` and
`OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` from `forge env up`; logs are read
from `.forge/logs/<env>/*.log`. Nothing to register, no key to copy.

Your `docker-compose.yml` and `deploy/kcl/dev/main.k` are yours, so forge did
not rewrite them. Until you do, the project keeps its old LGTM wiring. Take the
new shape from a fresh scaffold: `forge project new scratch --service x` in a
temporary directory, and copy from it.

## Steps

1. **`docker-compose.yml`.** Delete the `alloy` and `lgtm` services and the
   `lgtm_data` volume. Add the `clickstack` service, its three
   `clickstack_*` volumes, and the `otel-collector` network alias, exactly as in
   the scaffold. In `app` and `app-debug`, replace
   `OTEL_EXPORTER_OTLP_ENDPOINT: http://lgtm:4317` with
   `http://otel-collector:4318` and add `OTEL_EXPORTER_OTLP_PROTOCOL: http/protobuf`;
   change `depends_on: lgtm` to `clickstack`.
2. **`deploy/kcl/dev/main.k`.**
   - Replace the `_observability` block and `_otlp_port` with the scaffold's:
     `_observability = True` plus `_otlp_http_port`, `_otlp_grpc_port` and
     `_hyperdx_port`, each from `plugin.resolve_port`.
   - Delete the `_telemetry = {OTEL_EXPORTER_OTLP_ENDPOINT = ...}` env layer and
     its use in `_env`. `forge env up` exports the endpoint now.
   - Replace the `lgtm` workload in `_telemetry_workloads` with the `clickstack`
     one, passing the three `CLICKSTACK_*_PORT` values.
3. **New files.** Copy `deploy/observability/otel-collector.yaml` (the log
   pipeline) and `deploy/observability/dashboards/` (the dashboard provisioner
   directory) from the scaffold.
4. **Remove the dead files.** `deploy/alloy-config.alloy` and
   `deploy/observability/grafana/` are no longer written. `forge generate` lists
   them as stale; `forge generate --force-cleanup` deletes the untouched ones.
   Delete any you edited by hand.
5. `forge generate && forge env render dev`, then `forge env up dev`. The summary
   lists the HyperDX UI; `forge env status dev` reports traces, metrics and logs
   by service name.

## Things that changed under you

- **Grafana, Prometheus, Tempo, Loki and Pyroscope are gone from the default
  path.** `/metrics` on your app is untouched. Dashboards that lived in Grafana
  are not carried over; put HyperDX dashboard JSON in
  `deploy/observability/dashboards/`.
- **Profiles are not collected locally yet.** pprof stays on `127.0.0.1`.
- **Memory.** ClickStack is about 0.8 GB resident, in the same range as LGTM.

## To keep it off

`_observability = False` in `deploy/kcl/dev/main.k`. No container starts and no
endpoint is exported.

## Existing OTLP configuration wins

`forge env up` only fills `OTEL_EXPORTER_OTLP_ENDPOINT` and `_PROTOCOL` when they
are unset or empty. A value from your shell, `config.k`, or a workload's `env` is
left alone, so a project that already points at its own collector keeps doing so.
