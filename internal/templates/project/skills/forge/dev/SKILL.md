---
name: dev
description: Local-cluster dev loop primitives — cluster lifecycle, status, logs, ingress URLs, host/cluster split. Compose with project-specific bash for sibling-repo deploys, helm bootstraps, and webhook listeners.
---

# Forge Dev Loop

For local-dev-against-a-cluster workflows. For local-go-only (no k8s) see the
`forge` skill.

## Commands

| Command | What it does |
|---|---|
| `forge cluster up [--wait]` | Create k3d cluster from `deploy/k3d.yaml`. Idempotent — no-op if already up. |
| `forge cluster down` | Delete the cluster. Idempotent — no-op if not present. |
| `forge cluster reset` | Down then up (default `--wait=true`). |
| `forge cluster reload` | Re-render `deploy/kcl/dev/` + kubectl apply + wait rollout. The inner-loop reload after editing code or KCL. |
| `forge cluster status [--json]` | Cluster up/down + kubectl context + config path + pods in the dev namespace + ingress URLs + sibling dev namespaces. |
| `forge cluster logs [--service x] [--tail N]` | Stream `kubectl logs -f` for one or all forge-managed pods in the dev namespace. |
| `forge cluster info` | Diagnostic dump — cluster, context, namespace, registry, the component list (every server answers on the binary's one mux, `PORT`, default 8080) and declared frontend ports. |
| `forge cluster urls [--json]` | Print the ingress URL table for the dev env (one row per HTTP/GRPC route). |
| `forge cluster instances [--json]` | List every forge-managed dev namespace across every reachable k3d cluster (multi-worktree). |
| `forge env up <env> --target <service> [--background]` | Single-service runner: scopes the WHOLE run — build, deploy, host and frontend phases — to the named service. A service whose KCL declares a `host` block launches as a host process, dispatching on `host.runner` (`go-run` / `air` / `binary` / `delve`). |
| `forge env options <env> [--json]` | List the `-D` render options that env's KCL declares (see below). |
| `forge env config <env> [--json] [--workload <name>]` | Print the resolved configuration `deploy/kcl/<env>/` hands each workload — the values `forge env up` passes to each process (see below). |
| `forge env down <env> [--all]` | Stop this project's stack for that env, tracked or orphaned. `--all`: all of them, machine-wide. |
| `forge env ps` | Every stack running here: project dir, env, process count. |
| `forge env up <env> [--no-build] [--no-deploy] [--target <name>] [-D name=value] [--background]` | The whole-loop orchestrator: build (host-mode services filtered out) → cluster apply → host launch → frontend dev-serve. Reads `deploy/kcl/<env>/` to split services by provider. `--target` narrows WHICH entities each phase acts on; it never turns phases off. |
| `forge env deploy dev [--prune] [--target <app>]` | Apply `deploy/kcl/dev/`. Skips rollout wait for services declaring `deploy = forge.HostDeploy {...}`. `--prune` deletes orphan forge-managed Deployments. `--target <app>` (repeatable, by service/frontend name) deploys ONLY that app, keeping shared resources (Namespace, ConfigMap/Secret, RBAC) and dropping other apps' workloads. |

## Host vs cluster: where does each service run in dev?

Default is **cluster**: every service runs in k3d, reached from the host via the
Gateway API ingress path (`forge cluster urls` lists the routes). That is the
right shape for services needing cluster-only primitives — operators, CRD
watchers, ingress webhooks, sidecars depending on dynamic-config injection.

**Host mode** is DECLARED, never asserted from the command line. Set the deploy
target in `deploy/kcl/<env>/main.k` to `forge.HostDeploy` — per-env, typically
only in `dev`, with `staging` and `prod` staying on `forge.K8sCluster`. Then
`forge env up dev` launches it as a host process, and `--target <service>`
narrows the run to it.

There is no flag that reinterprets a cluster-declared service as a host
process. If a service should be host-runnable in dev, its KCL says so; to vary
HOW it launches, declare a render option and select it (`-D host_runner=go-run`
against a `host` block whose `runner` reads that option).

```kcl
# deploy/kcl/dev/main.k
import forge

_bundle = forge.Bundle {
    services = [
        forge.Service {
            name = "admin-server"
            deploy = forge.HostDeploy {
                runner = "air"
                air_config = ".air.toml"
                env_vars = [
                    forge.EnvVar { name = "DATABASE_URL", value = "postgres://..." }
                ]
            }
        }
        forge.Service {
            name = "workspace-controller"
            deploy = forge.K8sCluster {           # operator-shape — stays in cluster
                cluster = "k3d-myapp"
                namespace = "myapp-dev"
                registry = "localhost:5050"
            }
        }
    ]
    # Secret VALUES come from the bundle provider, not per-service.
    secret_provider = forge.FileSecrets { path = "secrets/dev.yaml" }   # gitignored YAML store
}
```

The decision rule:

| Service shape | Recommended dev deploy |
|---|---|
| Connect-RPC API, business logic, gateway | `forge.HostDeploy` |
| Operator (controller-runtime, watches CRDs) | `forge.K8sCluster` |
| Webhook ingress / TLS-terminating proxy | depends — `forge.K8sCluster` if it needs an Ingress, `forge.HostDeploy` if it's an upstream forwarder |
| Worker (background processor, cron) | `forge.HostDeploy` for fast iteration; `forge.K8sCluster` to test scheduler interactions |
| Anything that talks to the cluster API (e.g. `kubectl` shells) | `forge.K8sCluster` |

`forge env up staging` and `forge env deploy prod` see whatever each env's
`main.k` declares — typically every service on `forge.K8sCluster` regardless of
what dev does.

`HostDeploy` env composition layers two sources: the bundle-level
`secret_provider` is injected FIRST, then the service's KCL `env_vars` on top,
so KCL wins on conflict. See `secrets` for the provider model.

What flipping a service to host mode buys:

- `forge env deploy dev` skips its rollout wait (saves 120s/service).
- `forge env deploy dev --prune` deletes its stale in-cluster Deployment.
- `forge build dev` lists it under "host-mode services", to be run with
  `forge env up dev --target <name>` (or just `forge env up dev`).
- The scaffolded `cmd/<bin>/cmd/serve.go` operator-gating helper won't start the
  controller manager when the user filters to host-mode-only services.

## Inner loop: editing a host-mode service

`forge env up dev` is the one-command inner loop — infra up, cluster-mode
services applied, host-mode services launched, every frontend dev-served. It
also keeps two gitignored prerequisites fresh, each gated on staleness (a no-op
in the steady state):

- **Generated code** — runs `forge generate` when `gen/` is missing or
  `proto/` is newer than the generated tree (`--no-generate` to skip).
- **Frontend deps** — runs `<dev_runner> install` for a frontend whose
  `node_modules` is missing or older than its lockfile/manifest
  (`--no-install` to skip).

After a batch of proto edits, `forge scaffold` catches the tree up (see
`forge` and `db`). `forge run` and `forge env up` also auto-seed a
fresh dev DB from the applied schema on first boot, so a clean checkout comes up
with FK-coherent demo data — dev only; `--no-seed` opts out, `forge db seed
status` inspects.

For fine-grained control:

```bash
# Terminal 1: long-running infra + cluster services
forge cluster up --wait
forge env deploy dev

# Terminal 2: the service you're actively editing
forge env up dev --target admin-server                 # foreground; Ctrl-C to stop
# or detach + tail logs separately:
forge env up dev --target admin-server --background    # detach; PIDs tracked per env
forge env down dev                                     # later teardown
```

The host child process also inherits the host shell's env, so anything already
exported wins over both the injected `secret_provider` layer and `env_vars`.

## Render options: varying a run without editing KCL

To change something per run rather than per commit — which runner a host
service launches under, whether to point at a remote dependency — use a
**render option**. An env declares one by *reading* it, so the call site is the
declaration:

```python
# deploy/kcl/dev/main.k
_host_runner = option("host_runner", type="str", default="air",
                      help="Host launch runner: air (default) or go-run")
```

```
forge env options dev            # what this env declares
forge env up dev -D host_runner=go-run
```

forge does **not** interpret the value: it checks the *name* against what the
env declares (a typo fails instead of silently doing nothing) and relays the
value verbatim as a string. Your KCL decides what it means. Declare
`type` / `default` / `help` on `option()` — they are what `forge env options`
shows the next reader.

- **Options forge derives are refused.** `env`, `namespace`, `image_tag`,
  `image_digests`, `worktree`, `branch` are computed by forge; a caller-supplied
  value would disagree with what was actually built and applied.
- **`-D` is accepted on `env up` only**, never `env deploy`. A cluster apply
  has to be reproducible from the repo alone.

Because the KCL resolves the option, it can do what a CLI flag could not — the
canonical case being leaving Air, where `HostOverrides` forbids `air_config`
unless the runner is `air`, and the Air config is usually the only place the
service's real entrypoint is written down:

```python
host = forge.HostOverrides {
    runner = _host_runner
    if _host_runner == "air":
        air_config = ".air.api.toml"
    else:
        # what the Air config was carrying
        command_override = ["go", "run", "./cmd/api", "server", "api"]
}
```

## Reading an environment's resolved configuration

`forge env config <env>` prints what each workload is actually configured
with — the same values `forge env up` hands the processes it launches, with
launch-resolved ports reported as launched rather than as rendered.

```bash
forge env config dev                      # every workload, grouped
forge env config dev --workload api       # just one
forge env config dev --json               # machine-readable
```

**This is how you find the database (or the broker, or the bucket) you are
working on.** Do not go looking for it in `docker ps`: on a machine running
several projects that finds *a* postgres, not necessarily *this* project's, and
the mistake reads as correct right up until the schema disagrees. Do not
evaluate the KCL template string by hand either — an ephemeral port resolved at
launch lives in the run state, not in the source.

There is no `db dsn`-style command, since a project may run two databases, none,
or reach its store over something that is not a DSN. Select what you need out of
`env config`:

```bash
# whatever this project calls its database
psql "$(forge env config dev --json | jq -r '.workloads[].env.DATABASE_URL // empty' | head -1)"

# every `forge db` subcommand accepts an explicit --dsn, and otherwise
# falls back to $DATABASE_URL — so export it once and they all follow
export DATABASE_URL="$(forge env config dev --json | jq -r '.workloads[].env.DATABASE_URL // empty' | head -1)"
forge db seed status
```

## Logs & the `forge env up` summary

`forge env up <env>` writes every host service's and frontend's output
to a stable, greppable location:

```
.forge/logs/<env>/<service>.log
.forge/logs/<env>/frontend_<name>.log
```

This holds in **both** modes — foreground tees the file alongside the live
`[name]`-prefixed terminal stream, `--background` uses it as the sole sink. The
directory is gitignored (`.forge/*`). The path is project-relative and
deterministic, so read one service's output directly instead of scraping
interleaved scrollback:

```bash
tail -f .forge/logs/dev/admin-server.log
grep -i "error\|panic" .forge/logs/dev/*.log
```

After the host + frontend phases start, `up` prints a summary box listing each
process, its URL and its log path. Host-service URLs are derived from each
service's KCL `PORT` env var; a service that declares no `PORT` is listed
without one. Cluster service routes (Gateway API) are not host-local — list
them with `forge cluster urls`.

## Composing with Taskfile (cloud-dev pattern)

```yaml
# Taskfile.yml
tasks:
  dev:
    desc: Bring up cluster + cluster services, run host services locally
    cmds:
      - forge cluster up --wait
      - forge env deploy dev --prune       # cluster services only; host services pruned
      - forge env up dev --target admin-server --background
      - forge env up dev --target workspace-proxy --background

  dev-stop:
    cmds:
      - forge env down dev
```

## Safety: kubectl context pinning

Every `forge cluster` command runs against `k3d-<cluster-name>` (resolved from
`deploy/k3d.yaml` metadata.name, falling back to forge.yaml `name`), so you
cannot accidentally `forge cluster reload` into staging or prod.

`forge env deploy <env>` is DECLARATIVE-ONLY for cluster selection: the target
kubectl context comes SOLELY from the env's `forge.K8sCluster.cluster` in
`deploy/kcl/<env>/main.k`, threaded as `--context <declared>` on every kubectl
call. It never reads or falls back to your current kubectl context, and there is
no CLI override. Dev defaults to `k3d-<project>`; staging/prod declare it:

```kcl
# deploy/kcl/prod/main.k
import forge

_prod_k8s = forge.K8sCluster {
    cluster = "gke_acme-prod_us-central1_cluster-1"
    namespace = "myapp-prod"
}
```

The deploy fails fast (even under `--dry-run`) if the declared cluster has no
matching kubectl context. Fix your kubeconfig (e.g. `gcloud container clusters
get-credentials ...`) or correct `forge.K8sCluster.cluster` — there is no
`--context` escape hatch. `forge env deploy <env> --explain` prints the declared
context and whether it exists.

## Multi-worktree / multi-namespace

For per-worktree namespacing — each worktree its own namespace, one shared
cluster — set the `namespace` field on each worktree's `forge.K8sCluster` block
in `deploy/kcl/dev/main.k` (or via the `FORGE_DEV_NAMESPACE` env override if
your bootstrap supports it). `forge cluster instances` then lists every dev
namespace on the host with its pod count.

## What forge does NOT own

Forge owns the universal cluster + ingress + status mechanics. These stay in
`scripts/`, called from `Taskfile.yml` and composed with the `forge cluster`
primitives above:

- Sibling-repo deploys (project-specific helm installs, manifest applies)
- Helm chart bootstraps (project-specific stack — Postgres, Redis, observability)
- Webhook listeners (Stripe `stripe listen`, GitHub `gh webhook forward`, etc.)
- Project-specific DB seeding (custom schema + fixtures)
- Cross-service smoke tests (project-specific business invariants)

## CI usage

```bash
# guard: did we forget to run forge generate?
forge generate --check

# build + push to registry in one shot
forge build --push ghcr.io/acme

# deploy with context guard
forge env deploy staging
```

## When this skill is not enough

- Production deploy → see `deploy` skill (`forge env deploy <env>`)
- Greenfield setup → see `forge`
- Multi-cluster operator workflows → see `operators`
- Observability stack queries → see `observability`
