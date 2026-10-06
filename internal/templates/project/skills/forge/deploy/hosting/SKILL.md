---
name: deploy/hosting
description: The scaffolded staging/prod envs are hosted on Reliant — what `_hosted` / `_hosted_frontend` wire, rebinding a workload to a cluster you operate or a frontend to your own bucket, kinds hosting refuses (cron, operator), and how an existing cluster-bound project adopts hosting.
---

# Hosted by default

`forge project new` scaffolds `staging` and `prod` hosted on the forge control
plane (Reliant cloud). `dev` stays local (`forge.OnHost` + `forge.HostInfra`).

| In the env | What it is |
|---|---|
| `control_plane = forge.ControlPlane {}` | Reliant cloud; set `endpoint` for another control plane |
| `_db = forge.ManagedDatabase {runtime = forge.OnHosted {}}` | a dedicated Postgres, `deletionPolicy` RETAIN |
| `_hosted(wl.<name>)` | the workload on the platform (kinds service/worker/job) |
| `_hosted_frontend(_web_frontend)` | the static export on platform static hosting + CDN |
| `secret_provider = forge.HostedSecrets {}` | write-only store: `forge secret set --env <env> <KEY>` |

`_hosted` also:

- sets `image` to the declared image minus its registry host — the platform
  pulls only from its own registry, and forge composes
  `<registry>/<org>/<project>/<name>` from your credential (`deploy/hosted-registry`);
- sets `DATABASE_URL = forge.DatabaseRef {name = _db.name}` on every workload
  listing it in `config_secrets`;
- sets a service's `CORS_ORIGINS = forge.WorkloadURL {workload = "<site>"}`
  when the project was born with a frontend (`forge scaffold frontend` later
  prints the line to add).

`_hosted_frontend` sets the bare `image` and `runtime_config = {API_URL =
forge.WorkloadURL {workload = <first hosted service>}}`: the control plane
writes config.js after every sync, so one release promotes unchanged. The
frontend must be a static export (`output: static`, which the scaffold
writes). A frontend scaffolded by forge ≤ v0.1.43 has no `output:` (it is
standalone) and dynamic `[id]` pages: `forge skill load migrations/v0.1.44`.

Billing: hosted workloads and the managed database need it (Reliant →
Settings → Billing); a static site alone is free.

## Hosting elsewhere

The cluster and bucket binders are declared beside the hosted ones, unused,
with what to fill in shown in a comment:

```kcl
_cluster = forge.ClusterTarget {          # was: _cluster = None
    cluster = "gke_acme_us-central1_prod" # its kubectl context
    connected_cluster = "acme-prod"       # forge cluster connect acme-prod --context <ctx> --env prod
    namespace = "acme-prod"
    platform = "amd64"
}
_workloads = [
    _hosted(wl.migrate)
    _on_cluster(wl.search)                # rebound
]
```

The env still has a control plane, which deploys to the cluster too, so the
cluster is registered once (`forge cluster connect`) and named in
`connected_cluster`; the render refuses an unconnected one, naming the
command. An `_on_cluster` workload pulls the image workloads.k declares (push
it: `forge env build <env> --push`), gets the env's capacity floor, and reads
`DATABASE_URL` from the `<project>-secrets` Secret you provision in its
namespace (the managed database is reachable from the platform only). Moving
a whole env? Also add the commented `cluster_target`, `network_policy` and
gateway lines at the bottom of the Bundle; leaving hosting altogether, drop
`control_plane`, `_db`/`databases` and use `forge.ExternalSecrets {}` (then
no `connected_cluster`). A frontend: `_bucket = forge.OnBucket {bucket =
"..."}` and `_on_bucket(_web_frontend)`. Compose: `wl.<name> | {runtime =
forge.OnCompose {}}`.

A binding to `_on_cluster` / `_on_bucket` before its target is declared fails
the render, naming the workload and the fix. It is never a silent no-op.

## Kinds hosting refuses

An operator (no Kubernetes API on shared nodes) and a `kind = "cron"`
workload — a Kubernetes CronJob, not metered yet — cannot run hosted. forge's
own cron component (`forge scaffold worker <name> --kind cron`) is a worker
running its own scheduler, so it IS hosted.

`forge scaffold operator` binds the new workload `_on_cluster` in every
deployed env — the only binding that can run it — and warns, naming the envs,
that they refuse to render until `_cluster` is declared. The ways out:

1. declare `_cluster` — the env becomes mixed (hosted + your cluster);
2. drop the line from an env that should not run it (dev runs it on k3d).

The scaffold does not refuse the workload: its code is useful in dev, and a
render error at the binding is where the fix is made. Every other new
workload binds where the env's `migrate` job runs, so an env scaffolded on a
cluster keeps binding there.

## Adopting hosting in an existing project

Env files are scaffolded once and never rewritten, so a project scaffolded
before this default keeps its `_on_cluster` envs. To host one (try it beside
prod first: `forge env new cloud --from prod --bind <name>=hosted` derives the
env and adds the control plane):

1. In the Bundle: `control_plane = forge.ControlPlane {}`, `secret_provider =
   forge.HostedSecrets {}`, `databases = [_db]` with `_db =
   forge.ManagedDatabase {name = "<project>", runtime = forge.OnHosted {}}`.
2. In `_hosted`: `image = w.image[w.image.rfind("/") + 1:] if w.image else
   w.image`, and `DATABASE_URL = forge.DatabaseRef {name = _db.name}` in its
   env (or keep your own Postgres: `forge secret set --env <env> DATABASE_URL`).
3. Rebind each line `_hosted(wl.<name>)`; a frontend
   `_on_bucket(...)` → hosted (`image = "<name>"`, `runtime = forge.OnHosted {}`).
4. Drop the cluster-only fields (`cluster_target`, `network_policy`,
   gateways/routes, `ExternalSecrets`) once nothing is bound to the cluster.

`forge env render <env>` then `forge env new <env> --check` (renders, no
placeholder left, admissible to the control plane). The scaffold's current
shape: `forge project new` in a scratch dir, `deploy/kcl/prod/main.k`.
