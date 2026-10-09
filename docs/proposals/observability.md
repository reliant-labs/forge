# Proposal: observability by default

**Status:** revised, accepted for implementation (phase-1 decisions settled)
**Date:** 2026-10-08
**Scope:** Forge observability defaults and generated deployment contract, Reliant dogfood first, then hosted Forge users.

> The complete architecture, diagrams, signal matrix, security boundary, failure recovery, alternatives, dependency DAG, acceptance tests, and citations are in [`observability-architecture.md`](observability-architecture.md).

## Decision

Forge projects emit telemetry to a stable generated OTLP endpoint. Applications never know ClickHouse, HyperDX, Mongo, Pyroscope, tenant databases, or credentials. The official ClickStack collector owns the supported ClickHouse logs/traces/metrics schema.

Phase 1 uses the **official full ClickStack distribution** locally and for Reliant production dogfood: ClickHouse, HyperDX, the ClickStack collector, and MongoDB. This is an explicit choice over the lower-cost ClickHouse-only operations recommendation because phase 1 must validate HyperDX correlation and browser replay. HyperDX and Mongo are operator-only in dogfood, not a tenant boundary. If measured resource or operational cost fails the exit criteria, ClickHouse-only remains the fallback, not the architecture being validated.

Phase 1 has no Forge ingest gateway: one trusted internal tenant uses a private collector endpoint. A gateway is required before hosted multi-tenancy and will authenticate short-lived credentials, stamp tenant identity, enforce quotas, and route to isolated storage. Hosted MVP uses deployment/database/cluster isolation per tenant; pooling is deferred behind adversarial tests and external review.

Alloy remains for profiles: pprof and permitted eBPF collection go to Pyroscope. Alloy must not duplicate ClickStack logs or metrics. OTLP Profiles are alpha and not a ClickHouse feature. ClickStack captures correlated errors and replay, but Sentry remains for issue lifecycle, release/source-map workflows until proven, and Electron/native crashes.

## Required corrections to the previous proposal

- ClickStack's verified default store covers logs, traces, metrics, and sessions—not profiles or “all four signals.”
- The official ClickStack collector is distribution-specific; its exact component manifest must be pinned rather than described as a generic custom contrib build.
- Component licenses do not establish the bundled image BOM, trademark rights, hosted terms, or resale certainty; legal review is required for exact digests.
- Pooled row-policy tenancy is not an MVP default. HyperDX team-scoped Mongo state is not proof of ClickHouse row isolation.
- Alloy is not replaced wholesale: it remains the phase-1 profile path, while duplicate log/metric collection is removed.
- ClickStack is not a Sentry replacement today: issue grouping/workflow, release health, source-map guarantees, and native crash/minidump support remain unproven.
- HyperDX dashboard/alert APIs, import/export, revisioning, and rule-as-code are unstable/unverified. Repo-owned templates and a deliberately small semantic IR are the source of truth, with experimental projections.
- Resource and volume numbers are planning assumptions. Reliant load tests and billing data replace them before hosted commitments.

## Phases

1. **Pin and prove:** capture ClickStack/collector/ClickHouse/HyperDX/Mongo/Alloy/Pyroscope digests, component BOM, licenses, and compatibility tests. Run local full ClickStack with one Reliant workload.
2. **Reliant dogfood:** deploy the full stack privately, unify Reliant's tracer/resource/propagation setup with Forge's typed seam, validate traces, metrics, logs, browser replay, HyperDX search/waterfall, parallel Sentry errors, Alloy/Pyroscope profiles, backup/restore, bounded outage recovery, and independent meta-monitoring. Preserve LGTM/Grafana rollback.
3. **Forge defaults:** add typed backend-neutral `observability` config with working `endpoint: auto`, signal/sampling/redaction/retention/profile/error policy, generated compose/KCL/collector projections, and owned dashboard/alert overrides. Migrate `_observability` and legacy LGTM settings with explicit warnings for unsupported semantics.
4. **Dashboard projections:** keep current Grafana templates working; optionally project a small semantic dashboard/alert IR to pinned HyperDX APIs. Do not make HyperDX API behavior canonical.
5. **Hosted MVP:** add authenticated Forge gateway, credential lifecycle, immutable tenant stamping, quotas, deletion, audit, tenant-isolated deployment/database/cluster, and independent meta-monitoring. Run cross-tenant, forged-attribute, query, noisy-neighbor, restore, and erasure tests before external tenants.
6. **Later decisions:** evaluate HA ClickHouse/Keeper, managed services, pooled low-risk tenants, Forge console query UI, and error issue lifecycle only after measured cost, security, and API proof gates.

## First implementation slice

The first slice is **local full ClickStack plus a single Reliant workload**, followed by private Reliant production dogfood. Its acceptance tests are:

- known trace, metric, structured log, and error are queryable with real resource and trace identifiers;
- browser exception, trace propagation, replay masking, and CSP work through the pinned HyperDX SDK;
- HyperDX search/waterfall works while Sentry receives the parallel error;
- collector/ClickHouse outage produces bounded queues and visible no-data health without crashing the application;
- Alloy retrieves a pprof profile through the selected sidecar/proxy without port-forward;
- ClickHouse and Mongo backups restore in isolation;
- independent monitoring detects stopped ingestion;
- existing LGTM/Grafana remains a rollback path.

Implementation work is intentionally separated by repo/file boundary: Reliant instrumentation and dogfood inputs; control-plane ClickStack/profile deployment; Forge KCL/config and templates; dashboard projection; then hosted gateway/security/DR. No generated source is hand-edited. The architecture document contains the dependency DAG and exact boundary list.

## Declarative contract (shape)

```yaml
observability:
  enabled: true
  service_name: auto
  service_version: auto
  environment: auto
  endpoint: auto
  protocols: [grpc, http]
  signals: [traces, metrics, logs, browser_replay, profiles, errors]
  sampling: {traces: auto, errors: always, slow_traces: always, logs_success: auto}
  redaction: {preset: strict, fields: []}
  retention: {traces: 7d, metrics: 30d, logs: 14d, replay: 3d, profiles: 7d}
  profiles: {provider: pyroscope, pprof: true, ebpf: auto}
  errors: {sentry_parallel: true}
  dashboards: {defaults: true, overrides: observability/dashboards/}
  alerts: {defaults: true, overrides: observability/alerts/}
```

This is policy, not a blessed-mode enum. KCL types project it to dev and hosted deployments; generated files own derived wiring, while override directories remain user-owned. `endpoint: auto` always resolves to a working generated endpoint. `_observability: bool` migrates explicitly to `observability.enabled`; legacy LGTM settings migrate only where semantics match.

## Non-goals and blockers

No source code is changed by this proposal. Open owner decisions are the exact pinned release/digests, measured Reliant retention/sampling budgets, self-hosted versus managed hosted storage, Forge console timing, external review scope for pooling, and permanent native-crash/error ownership. These do not block phase 1; they block hosted pooling or Sentry retirement.

## Sources

See the architecture document for the complete source index. Phase 1 implementation inputs are `pkg/observe/setup.go`, `internal/templates/project/docker-compose.yml.tmpl`, `internal/templates/project/alloy-config.alloy.tmpl`, and `kcl/base.k`. Product and hosted implementation references are explicitly deferred to their owning repositories.
