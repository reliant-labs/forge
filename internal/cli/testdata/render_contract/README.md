# render contract goldens

One file per runtime, plus a mixed env: each is the JSON a
`deploy/kcl/<env>/main.k` ending in `output = forge.render(bundle)` evaluates
to (`{"output": {...}}`). This is the §9.1 contract between the KCL lowering
(`kcl/`, owner P2a) and its Go consumers (`internal/cli` decode, owner P2b;
KCL behaviour tests, owner P2c).

| File | What it pins |
|---|---|
| `host.json` | `forge.OnHost` workloads (air service with `listen_ports`, a go-run migrate job), a `HostInfra` infra entry, a dev frontend |
| `compose.json` | `forge.OnCompose` third-party services: no build, `spec.image` empty |
| `cluster.json` | `forge.OnCluster` workloads of every scheduled kind, a cluster `ManagedDatabase`, `network_policy`, and the `forge.dev/v1alpha1 Workload` records in `manifests` |
| `hosted.json` | `forge.OnHosted` service + job, a hosted database, a bucketless StaticSite; env refs (`managedSecret`, `databaseRef`, `workloadURL`) kept as references |
| `build-only.json` | `forge.BuildOnly` (`kind = tool`) with build variants, and a docker-built image |
| `mixed.json` | one env binding host, compose, cluster, hosted and build-only workloads side by side |

## Invariants

- `workloads[].spec` is `pkg/deploy/v1alpha1.WorkloadSpec` JSON and decodes
  with `DisallowUnknownFields`.
- Every `runtime.type == "cluster"` workload has exactly one
  `manifests[kind=Workload]` record, whose `spec` equals `workloads[].spec`
  byte for byte, and whose labels carry `forge.dev/cluster` (the kubectl
  context), `app.kubernetes.io/part-of` (the project) and `forge.dev/env`.
  No other runtime produces a record.
- `spec.image` is the resolved reference for a cluster workload
  (`<registry>/<image>:<tag>` or `@<digest>`), the bare artifact name for a
  hosted one (the hosted publish pins it to `repo@digest`), and `""` for
  host / compose / build-only.
- `forge.WorkloadURL` env is resolved to a `value` for host and cluster
  workloads, and kept as `{"workloadURL": {"name": ...}}` for hosted ones.
- `network_policy` is non-null iff some workload is cluster-bound (unless
  the Bundle overrides it); Go passes it as `deploy.Context.Network`.
- Optional values project as `null`, not omitted, where the key is part of
  the contract (runtime/build/infra/frontend blocks).

## Regenerating

v0 of these files was hand-written (`.scratch/p2a/gen_v0.py`) so the
consumers could start before the lowering existed. From v1 on they are
generated from the KCL fixtures beside them (`<name>.k`, one per golden):

    FORGE_UPDATE_GOLDEN=1 go test ./internal/codegen -run TestRenderContractGoldens

Without the variable the same test fails when `forge.render` of a fixture
differs from its golden. Review a regenerated diff as a contract change: a
changed key here is a change P2b's decoder and P2c's assertions must follow.
