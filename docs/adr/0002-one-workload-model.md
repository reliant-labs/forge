# ADR 0002: One workload model — declared once, bound to a runtime per workload

**Status:** accepted
**Supersedes:** the SimpleBackend tier as an authoring shape; `forge.Service`,
`forge.RenderedWorkload` (as authoring), `forge.Operator`, `forge.CronJob`,
`forge.OneShotJob`, `forge.HealthCheck`, `forge.HealthProbe`.

## Context

forge grew five ways to declare a backend (`fw.Workload`, `forge.Service`,
`RenderedWorkload`+`K8sCluster`, `RenderedWorkload`+`SimpleBackend`, and the
`Operator`/`CronJob`/`OneShotJob` siblings), three Kubernetes renderers
(`kcl/workloads/expand.k`, `kcl/lib/services.k`, Go `pkg/deploy`), and three
probe types. Each gap between them has shipped as a real defect:

- Hounders' hosted `api` ran with **no probes**. `SimpleBackendSpec.healthCheck` was
  optional with no default, and its single probe fed both liveness and
  readiness. The `fw.Workload` renderer gave the same app split
  `/readyz`/`/healthz` probes by default.
- The hosted renderer set **no termination grace period**. serverkit's
  default drain (5s pre-stop + 30s shutdown) exceeds Kubernetes' 30s default, so every
  rollout SIGKILLed pods mid-drain. `expand.k` had fixed this for its own path only.
- A scaffolded env declared every workload **twice**: `fw.render_workloads`
  for manifests, then a hand-written `Bundle.services` re-description for
  build/dispatch/host runs. The copies drifted (the host command was never
  derived from the workload).
- Hosting was **env-wide** (`Bundle.control_plane`). One env could not run
  one workload on the platform and another on a cluster. Yet real envs mix
  runtimes per workload: control-plane's `dev` runs air/go-run host
  processes, ../reliant host processes, k3d pods, compose infra and
  build-only images side by side.
- A hosted `migrate` job was **silently ignored**, so hosted apps migrated on
  boot through an app flag instead.

## Decision

### 1. One declaration: `fw.Workload`

Everything that runs is a `fw.Workload`: kind `service | worker | job | cron |
operator | tool`. It is declared once in `deploy/kcl/workloads.k` and shared
by every env. There is no other authoring shape for a runnable thing. The
schemas listed under _Supersedes_ are deleted. `forge.Frontend` (static sites)
and `forge.ManagedDatabase` stay: they are not processes forge runs. Both
still bind a runtime (§6).

### 2. The runtime is chosen per workload

A **runtime** is where a workload runs:

| Runtime                      | Meaning                                            |
| ---------------------------- | -------------------------------------------------- |
| `forge.OnHost {runner, ...}` | a local process (go-run, air, binary, delve)       |
| `forge.OnCompose {service}`  | a docker-compose service                           |
| `forge.OnCluster {target}`   | a Kubernetes cluster the user operates             |
| `forge.OnHosted {}`          | the forge control plane (Reliant cloud or another) |
| `forge.BuildOnly {}`         | built and pushed, never run                        |

`fw.Workload` carries `runtime?: Runtime`. An env binds workloads by
overlaying them, the same `|` mechanism it already uses for replicas and env:

```kcl
bundle = forge.Bundle {
    project = "hounders"
    workloads = [
        wl.membership | {runtime = forge.OnHost {runner = "air"}}
        wl.migrate    | {runtime = forge.OnHost {}}
        wl.search     | {runtime = forge.OnCluster {target = _k3d}}
    ]
}
```

Every workload states its own runtime; there is no env-level default. An env is a list of bindings, not a runtime. Env names mean nothing to forge: any env can use any runtime for any workload, and a self-hosted staging with a hosted prod, the reverse, or a mixed env are all just different bindings. `control_plane` is required only when some workload binds `OnHosted`.

A workload's runtime-independent facts (command, args, ports, env, probes,
build) are never re-stated per runtime. The host command is **derived** from
`build` plus `args`, and the cluster image from the build output.

### 3. The wire contract is a typed `Workload` CR; restriction is a profile

Go `pkg/deploy/v1alpha1.WorkloadSpec` is the single source of truth for what a
workload _is at runtime_: kind, image, command/args, replicas, resources, env,
ports, probes, storage, schedule, before, RBAC, CRDs, and so on. The KCL
`fw.Workload` schema is **generated** from it (plus the authoring-only fields
`name`, `build`, `runtime`), as `kcl/tiers/tiers_gen.k` already is today.

The hosted runtime accepts **only** `forge.dev/v1alpha1 Workload` CRs (plus
`StaticSite` and `ManagedDatabase`). It never accepts rendered Kubernetes
YAML. The control plane validates each CR server-side and renders it itself,
so hosted policy (Kata runtime class, isolation pool, registry allowlist,
secret scoping, routes, quota, egress) stays platform-owned and cannot be
authored by a hosted user.

**Restriction is a capability _profile_, not a separate app model.**
`Validate(spec, profile)`:

| Profile      | Where                                                           | Allows                                                                                                                                                                                                                  |
| ------------ | --------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Full`       | Cluster runtime; future per-customer hosted clusters (vcluster) | every field                                                                                                                                                                                                             |
| `Restricted` | Hosted on shared nodes (today)                                  | kinds `service`, `worker`, `job`; `replicas`, `resources`, `command`/`args`, `ports` (the platform routes `expose = true`), `probes`, `storage`, and env from `value` / `managedSecret` / `databaseRef` / `workloadURL` |

Restricted refuses RBAC, ServiceAccount annotations, CRDs/operators,
`configMapRef`/`fieldRef`/raw `secretRef` env, and `cron` (deferred until
metering covers it). Each error names the field and the reason. The profile
is chosen **server-side by the destination's isolation**, never by the
client. The CLI runs the same check at render time only so the author sees
the error before publishing.

**The profile is default-deny.** Every `WorkloadSpec` field is classified in
one table. A reflection test fails when a field is added to the spec without
a classification, so a new field can never silently become writable by a
hosted user.

Why a typed CR rather than restricted raw YAML: the spec is an allowlist by
construction, so a new Kubernetes PodSpec field is unreachable until we add
it; raw YAML makes the boundary a denylist over all of PodSpec that must
chase every Kubernetes release. The platform also needs to own the render in
order to compose isolation around it, and status (ready replicas, last-ready)
feeds billing. Hosted users never see the difference, because they author
`fw.Workload` either way. When each customer gets its own vcluster, the hosted
runtime runs the `Full` profile inside it. That is a profile switch, not a
new model.

### 4. One Kubernetes renderer, in Go

`pkg/deploy.RenderWorkloads(set, profile, ctx)` renders an env's workloads
for a cluster: Deployments, Services, Jobs, CronJobs, ServiceAccounts, RBAC,
NetworkPolicies, PDBs and PVCs. It is the only Kubernetes renderer. The CLI
runs it for `Cluster`-bound workloads; the control-plane operator runs it
(under `Restricted`) for `Hosted` ones. Its policies are the union of today's
best:

- readiness `/readyz` (fast, never restarts) plus liveness `/healthz`
  (timings derived from the readiness budget), defaulted for a service that
  forge built;
- `terminationGracePeriodSeconds` derived from the workload's drain env;
- soft topology spread plus a PDB above one replica;
- non-root, read-only rootfs with a `/tmp` emptyDir, no token automount
  unless the workload has Kubernetes RBAC;
- RBAC split by scope onto the workload's one ServiceAccount: a namespaced
  Role (the config-read defaults, an operator's leader-election lease, and
  `namespacedRBAC`) whenever it has any RBAC, plus a ClusterRole carrying
  only cluster-scoped intent (an operator's CRD rules, `clusterRBAC`). The
  two tiers add; the config-read defaults are never granted cluster-wide;
- `before` jobs as initContainers, plus standalone Jobs with a deploy phase;
- storage: `storageGiB > 0` requires 1 replica with a `Recreate` rollout.

Cross-workload semantics (`before`, `workloadURL`) resolve against the env's
set of workloads by this same function on both sides. `expand.k` and
`kcl/lib/services.k`'s Kubernetes renderers are deleted; KCL keeps
declarations only.

### 5. Probe policy is defined once

`WorkloadSpec.probes {port, readinessPath, livenessPath, timings}`. The
defaults live in Go:

- A service that forge built gets HTTP `/readyz` + `/healthz` on its `http`
  port. The lowering knows `build`, so it writes the probe into the spec
  explicitly.
- A service CR with ports but no probes gets TCP on its first port.
- A worker or job gets none unless it declares one.

### 6. Frontends bind a runtime too

`forge.Frontend` is not a process forge runs in production, so it is not a
`fw.Workload` (§1). It still has the property that made §2 necessary: WHERE
it is served differs per env. It used to be a `deploy` field whose ABSENCE
meant "dev server" and whose `forge.StaticSite` without a `bucket` meant
"hosted" — the hidden mode §2 removed from workloads. So a frontend binds a
runtime the same way, and it is required:

| Runtime                                        | Meaning                                                         |
| ---------------------------------------------- | --------------------------------------------------------------- |
| `forge.OnHost {}`                              | the dev server, `<dev_runner> dev` on the frontend's `port`     |
| `forge.OnHosted {}`                            | platform static hosting: a release artifact + a `StaticSite` CR |
| `forge.OnBucket {bucket, cdn?, keep_releases}` | the author's own object-storage bucket                          |
| `forge.OnFirebase {project, site, ...}`        | Firebase Hosting                                                |
| `forge.BuildOnly {}`                           | built for a sibling frontend's `bundle`, never shipped          |

`OnHost`, `OnHosted` and `BuildOnly` are the workload schemas, reused so one
name means one place across both declarations. The workload-only fields they
carry (`runner`, `listen_ports`, `build_variants`, ...) are refused on a
frontend at render, naming the field. The alternative — a parallel
`FrontendOnHost` — would give one runtime two names; the refusal costs one
lowering rule. `OnBucket` and `OnFirebase` are frontend-only.

The static BUILD facts (`public_dir`, `base_path`, `bundle`, `cache_control`)
are the frontend's own, never re-stated per runtime; `cache_control` is
refused where forge does not set the headers (everywhere but `OnBucket`).
`OnHosted` has no `bucket` field, so a hosted site that names one does not
compile. `control_plane` is required iff some workload, database or frontend
is `OnHosted`, and an env mixes frontend and workload runtimes freely. A
containerized (SSR) frontend is still a `fw.Workload` with a `DockerBuild`.

`forge env up` dev-serves every frontend whatever it binds: the runtime says
what a DEPLOY does with it, which is what lets `forge env up prod --target
web` run the prod bundle's config against a local dev server.

## Consequences

- One mental model for users: _declare a workload; bind it to a runtime_.
  The scaffold's env files shrink to bindings and per-env values.
- The control plane and the CLI render the same objects from the same code.
  Divergences like missing probes or grace periods cannot recur on one path only.
- control-plane renames the SimpleBackend operator to the Workload
  operator, and regenerates its CRD, proto mirrors, admission allowlist and
  observers. Its `deploy/kcl` migrates from `forge.Service`/`RenderedWorkload`
  to `fw.Workload` + runtimes.
- No backwards compatibility (pre-1.0). Existing hosted SimpleBackend CRs are
  replaced on the next deploy. `Frontend.deploy`, `forge.StaticSite` and
  `forge.FirebaseHosting` are deleted (§6); the `StaticSite` CR stays the
  hosted wire contract, and a hosted frontend publishes the same spec.
- Deferred, by design: `cron` on Hosted (metering), the `Full` profile on
  Hosted (vcluster), and cross-runtime reference resolution beyond
  `workloadURL`/`databaseRef` (e.g. a `PortOf` for host↔compose wiring).
