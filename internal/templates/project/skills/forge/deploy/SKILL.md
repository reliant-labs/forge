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
| `forge.OnCompose {service}` | a docker-compose service | `docker compose up` of that service (`shared = True`: from the primary checkout) |
| `forge.BuildOnly {}` | nowhere | builds and ships it (a CLI, an image others pull) |

A binding is the same `|` an env uses for any refinement. Env NAMES mean
nothing to forge — any env may bind any workload to any runtime, and one env
may mix them. This one runs `item` on the forge control plane and keeps
`migrate` and `search` on a cluster it operates:

```kcl
_prod = forge.ClusterTarget {cluster = "gke_acme_prod", namespace = "acme-prod", platform = "amd64"}

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
  so a rollout never SIGKILLs a pod mid-drain;
  `terminationGracePeriodSeconds` overrides it.
- **Rollout safety** above one replica: a PodDisruptionBudget and soft
  topology spread. Where your pods share nodes with pods that carry a
  PriorityClass, add `priorityClassName` too — an unranked pod is priority
  0, so it is preempted repeatedly and the rollout stalls in
  `FailedScheduling`. The class is cluster-scoped and must already exist on
  every cluster the workload lands on; a pod naming a missing one is
  refused at admission.
- **`strategy`** is how the Deployment replaces its pods. Unset is
  Kubernetes' `RollingUpdate`, which surges a new pod before the old one
  stops — so `replicas = 1` does NOT mean one process: two run for the
  length of every rollout. When the replica count is a correctness bound
  rather than a capacity choice (a sweeper that is not idempotent under
  concurrency), declare `strategy = "Recreate"` and accept the gap with no
  pod. Storage already forces Recreate, and `RollingUpdate` beside
  `storageGiB` is refused — the ReadWriteOnce volume would deadlock the
  surge pod.
- **Pod hardening**: non-root, read-only root filesystem with a `/tmp`
  emptyDir, no ServiceAccount token unless the workload declares RBAC.
- **`before` jobs** (`migrate` has `before = [fw.BEFORE_ALL]`) as an
  initContainer on every workload they gate.

### The hosted runtime is a restricted profile

A hosted workload shares nodes with other hosted users, so the control
plane admits only what it can run safely there. forge runs the same check
at render, so a refusal names the workload and field in your env file
rather than after a publish. Bind a refused workload to a cluster you
operate to keep it in a hosted env.

- **Allowed:** kinds `service`, `worker`, `job`; `replicas`, `resources`,
  `command`/`args`, `ports` (the platform routes the `expose = True` one),
  `probes`, `storageGiB`,
  `activeDeadlineSeconds`, `strategy`; env from a literal, `forge.ManagedSecret`,
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
  - `nodeSelector`, `tolerations`, `priorityClassName` — the platform
    decides placement, and ranks hosted pods on its own priority ladder.
  - `ports.domains` — a hosted hostname is a control-plane resource, not
    spec; see Custom domains.
  - Raw `secretRef`, `configMapRef` and `fieldRef` env — they address
    namespace objects you did not write.
  - A hostless or unpinned image — the release pins every artifact by
    digest.

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
forge build <env> --push          # and push each image to the reference its workload declares
forge registry login <env> -u <user> --password-stdin   # login to every host they name
forge registry ref <env>          # <image>@<digest> per image the last build pushed
forge build <env> --plan          # resolve + preflight the build set; build nothing
forge build --tag=<tag>           # override image tag (default: commit SHA)
forge build --debug               # with debug symbols for Delve
```

One project image carries every binary. Its ENTRYPOINT is the binary and
its CMD the default subcommand (`server`), so a workload's `args` select
what the pod runs, the same subcommand the host runtime runs.

A hosted env declares no registry either: each workload — and each hosted
frontend, via `forge.Frontend.image` — names its own.

### Docker build contexts

A `forge.DockerBuild` sends the PROJECT ROOT as its build context unless it
sets `context` (a project-root-relative directory) — set that when the
Dockerfile expects to run from its own directory, or the un-prefixed `COPY
package.json ./` fails as `failed to compute cache key: "/package.json": not
found`. Separately, `docker.build_contexts` declares NAMED contexts for
files outside the project tree, consumed via `COPY --from=<name>`. For both,
and which one a Dockerfile needs: load `deploy/build-contexts`.

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
every referenced Secret key and image exists. Hosted workloads publish to
the env's control plane, admitted there under the Restricted profile.

### Migrations run BEFORE the rollout

A standalone `kind = "job"` workload is **pre-rollout by default**: `forge
env deploy` applies it and waits for it to COMPLETE before any Deployment in
the same cluster group. If it fails, the deploy stops with no workload
changed and the error prints the exact `kubectl logs` command. The
scaffolded `migrate` goes further: `before = [fw.BEFORE_ALL]` runs it as an
initContainer on every workload, so no new pod serves against an old schema.
A job that needs this release's workloads running declares `deployPhase =
"post-rollout"`.

### Frontends bind a runtime too

A `forge.Frontend` states where it runs per env, like a workload — there is
no default, and a frontend with no `runtime` is a render error naming it:

| `runtime =`                                   | `forge env deploy` does                                                       |
| --------------------------------------------- | ----------------------------------------------------------------------------- |
| `forge.OnHost {}`                             | nothing (the dev server; `forge env up` runs it)                              |
| `forge.OnHosted {}`                           | publishes the site to the control plane's static hosting (below)             |
| `forge.OnBucket {bucket, cdn?, keep_releases}` | uploads to YOUR bucket: `releases/<digest>/` + `live/`, then CDN invalidation |
| `forge.OnFirebase {project, site, ...}`       | `firebase deploy`                                                             |
| `forge.BuildOnly {}`                          | builds it for a sibling frontend's `bundle`; ships nothing                    |

The build facts are the frontend's, the same on every runtime:
`public_dir` (default `out` for Next.js, `dist` otherwise), `base_path`,
`bundle`, `cache_control` (OnBucket only). A Next.js frontend published
statically needs `output: static` in forge.yaml; a server-rendered one is a
workload with a `forge.DockerBuild`.

```kcl
_web = forge.Frontend {name = "web", path = "frontends/web", public_dir = "out"}

frontends = [_web | {runtime = forge.OnBucket {bucket = "acme-prod-web"}}]
```

`forge env new cloud --from prod --bind web=hosted` rebinds a scaffolded
frontend's line (`_on_bucket(_web_frontend)` → `_hosted_frontend(...)`).
`forge env deploy <env> --frontends-only` ships only the bucket / Firebase
frontends.

### Hosted static sites

A frontend on `forge.OnHosted {}` publishes into the platform's bucket and
CDN, as an OCI release with **no `config.js` in it**, so `forge env promote`
moves one digest everywhere. `forge build <env> --push` pushes it. The
frontend's `runtime_config` becomes the spec's `runtimeConfig`:

```yaml
runtimeConfig:
  API_URL: {workloadURL: {name: item}}   # resolved by the control plane to the hosted item's URL
  APP_NAME: {value: acme}
```

After every sync the control plane writes `config.js`, resolving references
against workloads in the same environment.

### Custom domains

Every hosted site and exposed hosted port already answers on a hostname the
platform allocates. To ALSO serve your own, use the `forge domain` commands
— **a hosted domain is NOT spec**, and a hosted frontend or port carrying
`domains` is refused at render. `forge.OnCluster` is the exception and keeps
`Port.domains`. The commands, the reasoning and how to read a bound domain's
state: load `deploy/domains`.

## Pod priority on shared nodes

`priorityClassName` names a cluster-scoped PriorityClass that ranks a
workload's pods against every other pod competing for a node. Declare it
wherever your pods share nodes with pods that already carry one: an
unranked pod is priority 0, so a higher-priority pod preempts it, and a
rollout's surge pod can be evicted repeatedly before it ever runs.

```kcl
proxy = fw.Workload {name = "proxy", kind = "service", priorityClassName = "acme-platform"}
```

Apply the PriorityClass to every cluster the workload lands on BEFORE the
pods that name it — Kubernetes refuses a pod naming a class the cluster
does not have. Unset means the cluster's default. `forge.OnCluster` only:
the host and compose runtimes schedule nothing, and hosted ranks pods on
its own ladder.

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
<env>`, `forge env status <env>`. Verify exit 2 + `ledger BEHIND` = pull first.

**There is no rollback command, and that is deliberate.** A rollback claims
to undo a release, and it cannot: by the time you would run it the release
has applied its migrations and written rows in the new shape. So forge has
no `--rollback`, no `rollback_cmd`, and no `kubectl rollout undo` path.
Recovery is always a **new release that rolls forward**:

```bash
forge build prod --release v1.7.1 --push     # each image to its declared reference
forge env promote v1.7.1 --to prod --plan    # read it: direction must be AHEAD
forge env promote v1.7.1 --to prod --note "<incident>"
forge env deploy prod
```

Promote compare-and-sets against the plan's read: exit **3** = the env moved
meanwhile, nothing written — never retry blind. CI recipe: `forge env promote --help`.

A failed deploy changes nothing to undo: the pre-rollout gate stops a bad
migration before any workload changes, and a workload that never becomes
ready leaves the previous ReplicaSet serving. Binding an env to an OLDER
release is an ordinary promote labelled `direction BEHIND`; see
`db/deploy-migrations`.

## Rules

- Declare a workload once, in `workloads.k`. An env file that re-states a
  workload's command, ports or build is a second copy that will drift.
- Never `kubectl apply` a hand render — everything through `forge env deploy`.
  To read a VALUE KCL declares, `forge kcl eval`; see `deploy/kcl-eval`.
- Image tags are immutable — commit SHA by default, digest-pinned on deploy.
- Secrets never live in KCL — it is checked in. Reference them
  (`config_secrets`, `forge.SecretRef`, `forge.ManagedSecret`).
- EVERY image names its registry host (`ghcr.io/acme/api`). A hostless one is
  refused at render, naming the workload: no env registry exists to complete it.

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

## Reaching another cluster's API server

A workload that talks to a Kubernetes API server — a Flux controller, an
operator, a proxy — needs a kubeconfig in a Secret. `forge.KubeconfigSecret`
mints one on every deploy, so nothing stale is ever committed.

There are two credential models, and picking the wrong one produces a
kubeconfig that renders fine and cannot authenticate.

**Copying (the default).** With no `service_account`, forge copies the
credential from the target's own kubeconfig. That works for k3d, whose
kubeconfig carries a client certificate usable from anywhere:

```kcl
forge.KubeconfigSecret {
    name = "workload-kubeconfig"
    in_cluster = _cp.context        # where the Secret lands
    target_cluster = "workload"     # the cluster to reach (k3d)
    context_name = "workload"
}
```

**Minting (any cluster, including GKE).** A managed cluster's kubeconfig
authenticates with an `exec` plugin — "run `gke-gcloud-auth-plugin` using the
operator's credentials." Copy that into a Secret and the pod holding it has
no plugin binary and no credentials, so it cannot authenticate at all.
Declare `service_account` and forge mints a credential instead: a
ServiceAccount, the rules you declare, and a long-lived token, all created on
the TARGET cluster, projected into a kubeconfig whose only credential is that
token, inline.

```kcl
forge.KubeconfigSecret {
    name = "hub-kubeconfig"
    in_cluster = "gke_acme_us-central1_prod"     # where the Secret lands
    target_cluster = "prod"                       # label for the target
    target_context = "gke_acme_us-central1_prod"  # the context addressing it
    context_name = "hub"
    reachability = "in-cluster"                   # the reader is a pod HERE
    service_account = forge.KubeconfigServiceAccount {
        name = "flux-hub-applier"
        namespace = "acme-prod"
        rules = [
            forge.KubeconfigPolicyRule {
                api_groups = ["forge.dev"], resources = ["*"], verbs = ["*"]
            }
            forge.KubeconfigPolicyRule {
                api_groups = [""]
                resources = ["namespaces", "serviceaccounts"]
                verbs = ["get", "list", "watch", "create", "update", "patch"]
            }
        ]
    }
}
```

`reachability` says where the READER sits, which is what picks the
kubeconfig's `server`:

| value | the reader is | server |
|---|---|---|
| `in-network` (default) | a pod on the shared docker network | the k3d container by name, verified against the cluster CA |
| `endpoint` | outside the target | the target's own advertised endpoint |
| `in-cluster` | a pod INSIDE the target | `https://kubernetes.default.svc` |

`in-cluster` requires `service_account`: a pod cannot present the operator's
credential whatever the address.

Scope the grant with `namespaces` on the ServiceAccount — the rules become a
Role in each listed namespace instead of a cluster-wide ClusterRole. Prefer
that whenever the consumer's reach is known.

Three things worth knowing before you rely on it:

- **Rotation.** The token is long-lived (a `kubernetes.io/service-account-token`
  Secret, not a TokenRequest), so it does not expire between deploys and there
  is no refresh deadline to miss. To rotate, delete the token Secret on the
  target and deploy. A bounded TokenRequest ties credential validity to deploy
  cadence, so an env nobody deploys for a month stops working — bound the
  credential with `rules` instead, which is the control that matters.
- **Shared clusters.** Everything forge creates is labelled
  `app.kubernetes.io/managed-by: forge`, and forge REFUSES to adopt an
  existing object without it rather than rewrite permissions on something
  someone else owns.
- **Review before granting.** `forge env deploy --dry-run` prints every
  object the mint would create, on which cluster, without contacting one.

## `features:` block — disabling subsystems

`forge.yaml`'s `features:` block gates `deploy`, `build`, `ci`, `codegen`,
`orm`, `migrations`, `frontend`, `observability`, `hot_reload`,
`contracts`, `docs`, `ingress`, `operators` — plus experimental
`strict_wiring`, `reconcile` under `features.experimental:`. Each defaults
from the derived shape; explicit `features.<name>` wins. `ingress` and
`operators` derive false (opt-in).

## k3d local-registry mirror

Load the `deploy/k3d-registry` skill for the `localhost:5050` ↔
`registry.localhost:5000` containerd mirror.
