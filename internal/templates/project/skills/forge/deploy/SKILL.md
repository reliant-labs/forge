---
name: deploy
description: Ship code — declare workloads once, bind each one to a runtime per env (host, cluster, hosted), build, deploy, verify, roll forward.
---

# Ship It

## The model: declare once, bind per env

Everything that runs is a `fw.Workload`, declared ONCE in
`deploy/kcl/workloads.k` with the facts that are true wherever it runs:

```kcl
item = fw.Workload {
    name = "item"
    kind = "service"
    build = forge.GoBuild {cmd = "./cmd/acme", output_name = "acme"}
    args = ["item"]                                   # the subcommand, on every runtime
    ports = [fw.Port {name = "http", port = 8080, expose = True}]
    config_secrets = ["DATABASE_URL"]                 # the credentials it reads
}
```

Each env's `deploy/kcl/<env>/main.k` BINDS every workload to a runtime —
one binding per workload — and states only its own values. It never
re-describes a workload, and there is no env-level runtime: an env is a list
of bindings.

| Runtime | Where it runs | forge does |
|---|---|---|
| `forge.OnHost {runner}` | a local process | `forge env up` runs `go run ./cmd/<p> <args>` (or air / a built binary / delve) |
| `forge.OnCluster {target}` | a Kubernetes cluster you operate | renders a `forge.dev/v1alpha1 Workload` record and expands it through `pkg/deploy.RenderWorkloads` (Full profile) |
| `forge.OnHosted {}` | the forge control plane | publishes the spec as a Workload CR; the platform renders it (Restricted profile) |
| `forge.OnCompose {service}` | a docker-compose service | `docker compose up` of that service |
| `forge.BuildOnly {}` | nowhere | builds and ships it (a CLI, an image others pull) |

A binding is the same `|` an env uses for any refinement. Env NAMES mean
nothing to forge — any env may bind any workload to any runtime, and one env
may mix them. This env runs `item` on the forge control plane and keeps
`migrate` and `search` on a cluster it operates:

```kcl
_prod = forge.ClusterTarget {cluster = "gke_acme_prod", namespace = "acme-prod", registry = "ghcr.io/acme", platform = "amd64"}

output = forge.render(forge.Bundle {
    project = "acme"
    control_plane = forge.ControlPlane {}              # needed because item is hosted
    workloads = [
        wl.migrate | {runtime = forge.OnCluster {target = _prod}}
        wl.search  | {runtime = forge.OnCluster {target = _prod}}
        wl.item    | {runtime = forge.OnHosted {}}
    ]
})
```

A workload with no runtime is a render error naming it.

`output = forge.render(bundle)` is the ONE entrypoint. There is no
separate manifest stream: the cluster objects are Workload records only
forge can expand, so always render and deploy through forge
(`forge env render <env>`, `forge env deploy <env>`), never `kcl run |
kubectl apply`.

### The scaffolded envs

Each scaffolded env declares small binders — lambdas that apply the env's
layer (env vars, capacity) and one runtime — and lists every workload with
its own:

```kcl
# deploy/kcl/prod/main.k
_workloads = [
    _on_cluster(wl.migrate)
    _on_cluster(wl.item)
]
```

| Env | Binders | Notes |
|---|---|---|
| `dev` | `_on_host` / `_on_host_job` (services, workers, jobs), `_on_k3d` (operators, crons: a host process cannot be one) | host-run postgres (and dev IdP) via `forge.HostInfra` |
| `staging`, `prod` | `_on_cluster`; `_hosted` ready to use | `fw.Resources` + a replica floor on cluster workloads, `network_policy = forge.NetworkPolicy {...}` opted in, `forge.ExternalSecrets` |

Rebinding is editing one line: `_hosted(wl.item)` runs item on the control
plane (add `control_plane = forge.ControlPlane {}` to the Bundle). `forge
scaffold <kind>` appends the new workload's binding to every env.

Add another env by deriving it — each binding is copied, and the dangerous
per-env knobs (cluster context, platform, namespace) become REPLACE_ME
placeholders:

```
forge env new preview --from staging                        # same bindings as staging
forge env new cloud --from prod --bind item=hosted          # item on the control plane
forge env new cloud --check                                 # no placeholder left, renders, admissible
```

### What forge writes into every spec

- **Probes.** A service forge builds gets `/readyz` readiness and
  `/healthz` liveness on its `http` port, written explicitly into the spec
  on every runtime. That is what the scaffolded server serves. Declare
  `probes` only to change them. A third-party image with ports gets TCP on
  its first port; a worker or job gets none unless it declares one.
- **Grace period** from the drain env (`PRE_STOP_DELAY + SHUTDOWN_TIMEOUT`),
  so a rollout never SIGKILLs a pod mid-drain.
- **Rollout safety** above one replica: a PodDisruptionBudget and soft
  topology spread.
- **Pod hardening**: non-root, read-only root filesystem with a `/tmp`
  emptyDir, no ServiceAccount token unless the workload declares RBAC.
- **`before` jobs** (`migrate` has `before = [fw.BEFORE_ALL]`) as an
  initContainer on every workload they gate.

### The hosted runtime is a restricted profile

A hosted workload shares nodes with other hosted users, so the control
plane admits only what it can run safely there. forge runs the same check
at render, so a refusal names the workload and field in your env file
rather than after a publish.

- **Allowed:** kinds `service`, `worker`, `job`; `replicas`, `resources`,
  `command`/`args`, `ports` (the platform routes the `expose = True` one,
  including custom `domains`), `probes`, `storageGiB`,
  `activeDeadlineSeconds`; env from a literal, `forge.ManagedSecret`,
  `forge.DatabaseRef` or `forge.WorkloadURL`. A config-projected
  `forge.SecretRef` lowers to a `forge.ManagedSecret` of the same store key
  automatically.
- **Refused, and why:**
  - `cron` — not metered yet.
  - `operator`, `namespacedRBAC`, `clusterRBAC`, `crds`, ServiceAccount
    fields — no Kubernetes API access on shared nodes.
  - `sidecars`, `volumes`, `securityContext`,
    `terminationGracePeriodSeconds`, `podAnnotations` — the platform
    composes the pod and owns its identity and grace period.
  - `nodeSelector`, `tolerations` — the platform decides placement.
  - Raw `secretRef`, `configMapRef` and `fieldRef` env — they address
    namespace objects you did not write.
  - A registry-less or unpinned image — the release pins every artifact by
    digest.

A workload the hosted runtime refuses can still live in a hosted env:
bind it to a cluster you operate (`| {runtime = forge.OnCluster {target =
...}}`).

## Pre-flight checks

```
forge lint              # Go + proto + frontend linters (same checks CI runs)
forge lint --no-fix     # gate only, mutate nothing (CI / read-only)
task test               # full test suite must pass
forge ci validate-kcl   # every env renders to something applyable
forge doctor --signal deploy   # probes, resources, Secrets, migrations
```

## Build

```
forge build <env>                 # what <env> declares (host workloads need no image)
forge build <env> --push          # and push to the registry the env's ClusterTarget declares
forge registry login <env> -u <user> --password-stdin   # docker login to that registry
forge registry ref <env>          # <registry>/<image>@<digest> of the last pushed build
forge build <env> --plan          # resolve + preflight the build set; build nothing
forge build --tag=<tag>           # override image tag (default: commit SHA)
forge build --debug               # with debug symbols for Delve
```

One project image carries every binary. Its ENTRYPOINT is the binary and
its CMD the default subcommand (`server`), so a workload's `args` select
what the pod runs, the same subcommand the host runtime runs.

A hosted env declares its registry on `forge.ControlPlane` (`registry = "<registry-host>/<org>"`); `forge build <env> --push` pushes there.

### Multi-source Docker builds (`docker.build_contexts`)

When a Dockerfile needs files from outside the project tree (a sibling
checkout the `go.mod` `replace`s against, a shared-libs monorepo sibling,
a base image to pin), declare the extra contexts in `forge.yaml`:

```yaml
docker:
  build_contexts:
    shared: ../shared-libs            # relative path, resolved against forge.yaml's dir
    base: docker-image://acme/base:v3  # registry image — pin or local-override a FROM
```

Consume them via `FROM <name>` or `COPY --from=<name>`. Each entry becomes a
`docker buildx --build-context name=value` arg.

## Deploy

Environment is a positional arg — `forge env deploy dev`, not `forge env deploy --env dev`.

```
forge env render prod             # every object, and the cluster it lands on — changes nothing
forge env deploy prod             # build state → pinned digests → apply / publish → rollout wait
forge env deploy prod --dry-run   # render/print without applying
forge env deploy prod --target item   # one workload
```

Each workload deploys through its runtime: cluster workloads are applied to
the kubectl context the ClusterTarget names (forge refuses when it is
missing rather than using the current one), after a live preflight that
every referenced Secret key and image exists. Hosted workloads are
published to the env's control plane, admitted there under the Restricted
profile.

### Migrations run BEFORE the rollout

A standalone `kind = "job"` workload is **pre-rollout by default**: `forge
env deploy` applies it and waits for it to COMPLETE before it applies any
Deployment in the same cluster group. If it fails, the deploy stops with no
workload changed and the error prints the exact `kubectl logs` command. The
scaffolded `migrate` goes further: `before = [fw.BEFORE_ALL]` runs it as an
initContainer on every workload, so no new pod serves against an old
schema. A job that needs this release's workloads running declares
`deployPhase = "post-rollout"`.

### Hosted static sites

On a hosted env a `forge.StaticSite` with no `bucket` is published into the
platform's bucket and CDN, as an OCI release with **no `config.js` in it**,
so `forge env promote` moves one digest everywhere. The frontend's
`runtime_config` becomes the spec's `runtimeConfig`:

```yaml
runtimeConfig:
  API_URL: {workloadURL: {name: item}}   # resolved by the control plane to the hosted item's URL
  APP_NAME: {value: acme}
```

After every sync the control plane writes `config.js`, resolving references
against workloads in the same environment.

## forge env up — the local loop

```
forge env up dev              # build + cluster apply + host launch + frontend dev
forge env up dev --target item
forge env up dev -D name=value    # a render option the env's KCL declares (forge env options dev)
```

Host workloads start in `before` order (`migrate` to completion first),
each on the port its env declares; frontends run their dev server with the
declared port force-injected as `PORT`.

## Verify + recover — roll forward, never back

After every deploy confirm the env runs what it claims: `forge env verify
<env>`, `forge env status <env>`.

**There is no rollback command, and that is deliberate.** A rollback claims
to undo a release, and it cannot: by the time you would run it the release
has applied its migrations and written rows in the new shape. So forge has
no `--rollback`, no `rollback_cmd`, and no `kubectl rollout undo` path.
Recovery is always a **new release that rolls forward**:

```bash
forge build prod --release v1.7.1 --push     # the registry prod's KCL declares
forge env promote v1.7.1 --to prod --plan    # read it: direction must be AHEAD
forge env promote v1.7.1 --to prod --note "<incident>"
forge env deploy prod
```

A failed deploy changes nothing to undo: the pre-rollout gate stops a bad
migration before any workload changes, and a workload that never becomes
ready leaves the previous ReplicaSet serving. Binding an env to an OLDER
release is an ordinary promote labelled `direction BEHIND`; see
`db/deploy-migrations`.

## Rules

- Declare a workload once, in `workloads.k`. An env file that re-states a
  workload's command, ports or build is a second copy that will drift.
- Never `kubectl apply` a hand render — everything through `forge env deploy`.
- Image tags are immutable — commit SHA by default, digest-pinned on deploy.
- Secrets never live in KCL — it is checked in. Reference them
  (`config_secrets`, `forge.SecretRef`, `forge.ManagedSecret`).
- A third-party image names its registry host (`docker.io/library/nats:2.10`);
  a registry-less `image` means forge's own artifact.

## Per-env config — KCL is the surface

Typed per-env config lives in `deploy/kcl/<env>/config.k` (an `AppConfig`
instance); `config_gen.appConfigEnvMap(cfg, w.config_secrets)` projects it
into a workload's `env` map. A `sensitive` field reaches only the workloads
that list it in `config_secrets`, so one feature's missing Secret key cannot
stall every pod. Anything else is a literal in the workload's `env`:

```kcl
wl.item | {env = {LOG_LEVEL = "debug"}}
```

## Cross-references — declare once, read twice

When two fields must agree (a route's port and the service's, an issuer in
two places), declare the value on the schema that OWNS it and reference it
from the other. A `forge.WorkloadURL {workload = "item"}` in an env or a
frontend's `runtime_config` is resolved by forge (host, cluster) or by the
control plane (hosted), so a URL is never written twice. A port mismatch is
invisible at render and fatal at runtime — make it impossible.

## Extra Kubernetes objects

Anything that is not a workload — a CRD, a ClusterIssuer, a vendored chart's
output — goes in `Bundle.manifests`, placed explicitly:

```kcl
manifests = [forge.Manifests {name = "issuer", cluster = "gke_acme_prod", objects = [_issuer]}]
```

A hosted env never accepts raw objects: the platform owns the render.

## `features:` block — disabling subsystems

`forge.yaml`'s `features:` block gates `deploy`, `build`, `ci`, `codegen`,
`orm`, `migrations`, `frontend`, `observability`, `hot_reload`,
`contracts`, `docs` — plus experimental `ingress`, `external_builds`,
`operators`, `strict_wiring` under `features.experimental:`. Each defaults
from the project's derived shape; an explicit `features.<name>:
true|false` wins.

## k3d local-registry mirror

Load the `deploy/k3d-registry` skill for the `localhost:5050` ↔
`registry.localhost:5000` containerd mirror.
