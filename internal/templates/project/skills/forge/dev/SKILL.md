---
name: dev
description: Local-cluster dev loop primitives — cluster lifecycle, port-forward, status, logs, host/cluster split. Compose with project-specific bash for sibling-repo deploys, helm bootstraps, and webhook listeners.
---

# Forge Dev Loop

For local-dev-against-a-cluster workflows. For local-go-only (no k8s) see the
`getting-started` skill.

## Commands

| Command | What it does |
|---|---|
| `forge dev cluster up [--wait]` | Create k3d cluster from `deploy/k3d.yaml`. Idempotent — no-op if already up. |
| `forge dev cluster down` | Delete the cluster. Idempotent — no-op if not present. |
| `forge dev cluster status [--json]` | Show cluster up/down, kubectl context, config path. |
| `forge dev cluster reset` | Down then up (default `--wait=true`). |
| `forge dev cluster reload` | Re-render `deploy/kcl/dev/` + kubectl apply + wait rollout. The inner-loop reload after editing code or KCL. |
| `forge dev status [--json]` | Cluster + pods in dev namespace + active port-forwards + sibling dev namespaces. |
| `forge dev logs [--service x] [--tail N]` | Stream `kubectl logs -f` for one or all forge-managed pods in the dev namespace. |
| `forge dev info` | Diagnostic dump — cluster, context, namespace, registry, declared service/frontend ports. |
| `forge dev port-forward` | Forward every service in `forge.yaml` (parallel). Ctrl-C cleans up. PIDs in `~/.cache/forge/dev/<cluster>/<ns>.pids`. |
| `forge dev instances [--json]` | List every forge-managed dev namespace across every reachable k3d cluster (multi-worktree). |
| `forge run <service> [--background]` | Host-mode single-service runner. Dispatches on KCL `Service.deploy.runner` (`go-run` / `air` / `binary` / `delve`), loading `HostDeploy.secrets_file` first then layering `env_vars` on top. For services whose dev env declares `deploy = forge.HostDeploy {...}`. |
| `forge run <service> stop` | Kill the background process tracked by `forge run <service> --background`. |
| `forge up --env=<env> [--no-build] [--no-deploy] [--cluster-only] [--host-only] [--background]` | The whole-loop orchestrator: build (host-mode services filtered out) → cluster apply → host launch → frontend dev-serve. Reads `deploy/kcl/<env>/` to split services by provider. |
| `forge deploy dev [--prune]` | Apply `deploy/kcl/dev/`. Skips rollout wait for services declaring `deploy = forge.HostDeploy {...}`. `--prune` deletes orphan forge-managed Deployments. |

## Host vs cluster: where does each service run in dev?

Default is **cluster**: every service runs in k3d, reached via
`forge dev port-forward`. This is the right shape for services that need
cluster-only primitives — operators, CRD watchers, ingress webhooks,
sidecars that depend on dynamic-config injection.

**Host mode** flips a service to run as a host process under `forge run
<service>`. Set the deploy target in `deploy/kcl/<env>/main.k` to
`forge.HostDeploy` (per-env — typically only in `dev`, with `staging` and
`prod` staying on `forge.K8sCluster`):

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
                secrets_file = ".env.dev.local"   # gitignored
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

The host/cluster split is a per-env concern. `forge up --env=staging` and
`forge deploy prod` see whatever each env's `main.k` declares — typically
every service on `forge.K8sCluster` in staging / prod regardless of what
dev does.

`HostDeploy` env composition splits config from secrets:

- `env_vars` — KCL-declared per-env config (DATABASE_URL, NATS_URL,
  LOG_LEVEL, …). Reproducible, version-controlled, composes with
  the same sources `K8sCluster` services see via the Deployment's env block.
- `secrets_file` — gitignored dotenv with JUST the secrets (STRIPE_*,
  SUPABASE_*, JWT_PUBLIC_KEY, …). Loaded first; `env_vars` layers on top
  so KCL wins on conflict and per-env config can't drift between
  developer machines.

What flipping a service to host mode buys:

- `forge deploy dev` skips its rollout wait (saves 120s/service).
- `forge deploy dev --prune` deletes its stale in-cluster Deployment.
- `forge build --env=dev` lists it under "host-mode services" so users know
  they need to run it with `forge run <name>` (or just `forge up --env=dev`).
- The scaffolded `cmd/server.go` operator-gating helper won't start the
  controller manager when the user filters to host-mode-only services
  (no more spurious "not running in-cluster" errors during a host run).

## Inner loop: editing a host-mode service

`forge up --env=dev` is the one-command inner loop — it brings up infra,
applies the cluster-mode services, launches the host-mode services, and
dev-serves every frontend. Use the breakdown below when you want
fine-grained control:

```bash
# Terminal 1: long-running infra + cluster services
forge dev cluster up --wait
forge deploy dev
forge dev port-forward &

# Terminal 2: the service you're actively editing
forge run admin-server                  # foreground; Ctrl-C to stop
# or detach + tail logs separately:
forge run admin-server --background     # PID at ~/.cache/forge/run/admin-server.pid
forge run admin-server stop             # later teardown
```

`forge run admin-server` reads the service's `HostDeploy.secrets_file`
(if declared) FIRST, then layers `HostDeploy.env_vars` on top — KCL
wins on conflict so per-env config can't drift between developer
machines. Override the secrets-file path with `--env-file`. The child
process also inherits the host shell's env, so anything already
exported wins over both.

## Composing with Taskfile (cloud-dev pattern)

The host/cluster split makes the canonical `task dev` shape:

```yaml
# Taskfile.yml — mirrors source's `make run-admin`
tasks:
  dev:
    desc: Bring up cluster + cluster services, run host services locally
    cmds:
      - forge dev cluster up --wait
      - forge deploy dev --prune       # cluster services only; host services pruned
      - forge dev port-forward --background
      - forge run admin-server --background
      - forge run workspace-proxy --background

  dev-stop:
    cmds:
      - forge run admin-server stop
      - forge run workspace-proxy stop
      - forge dev port-forward stop
```

## Safety: kubectl context pinning

Every `forge dev` command runs against `k3d-<cluster-name>` (resolved from
`deploy/k3d.yaml` metadata.name, falling back to forge.yaml `name`). This means
you cannot accidentally `forge dev cluster reload` into staging or prod.

`forge deploy <env>` enforces the same guard: before applying, it verifies the
current kubectl context matches the env's `forge.K8sCluster.cluster` declared
in `deploy/kcl/<env>/main.k`. For dev this defaults to `k3d-<project>`; for
staging/prod declare the expected context explicitly:

```kcl
# deploy/kcl/prod/main.k
import forge

_prod_k8s = forge.K8sCluster {
    cluster = "gke_acme-prod_us-central1_cluster-1"
    namespace = "myapp-prod"
}
```

CI deploy-bots that legitimately target multiple envs from one context use
`forge deploy prod --context <name>` to override the guard.

## When the project needs more

`cloud-dev` / `cluster-bootstrap` scripts that deploy sibling repos, install
helm charts, run Stripe webhook listeners, or seed per-tenant DBs — keep those
in `scripts/` and `Taskfile.yml`. Forge owns the universal cluster +
port-forward + status mechanics; the project owns the project-specific
orchestration. Compose them:

```yaml
# Taskfile.yml
tasks:
  cloud-dev:
    desc: Full dev loop with sibling deploys
    cmds:
      - forge dev cluster up --wait
      - task deploy-reliant       # your bash — sibling-repo helm install
      - task ensure-litellm-db    # your bash — out-of-band DB bootstrap
      - forge deploy dev          # forge KCL apply (with context guard)
      - forge dev port-forward &
      - task stripe-listen &
      - wait
```

## Multi-worktree / multi-namespace

`forge dev instances` lists every dev namespace on the host — designed for
projects using per-worktree namespacing (each worktree gets its own namespace
sharing one cluster). The pattern:

```bash
$ forge dev instances
CLUSTER             NAMESPACE                           PODS   PORT-FORWARDS
cp-forge            cp-forge-dev                        12     4
cp-forge            cp-forge-dev-feat-billing           12     0
cp-forge            cp-forge-dev-fix-auth               12     0
```

Each worktree sets the `namespace` field on its `forge.K8sCluster` block
in `deploy/kcl/dev/main.k` (or via the `FORGE_DEV_NAMESPACE` env override
if supported by your bootstrap) so multiple worktrees can run concurrently
against one shared cluster.

## What forge does NOT own

- Sibling-repo deploys (project-specific helm installs, manifest applies)
- Helm chart bootstraps (project-specific stack — Postgres, Redis, observability)
- Webhook listeners (Stripe `stripe listen`, GitHub `gh webhook forward`, etc.)
- Per-tenant DB seeding (project-specific schema + fixtures)
- Cross-service smoke tests (project-specific business invariants)

Keep these in `scripts/` and call them from `Taskfile.yml`. Compose them with
`forge dev` primitives.

## CI usage

```bash
# guard: did we forget to run forge generate?
forge generate --check

# build + push to registry in one shot
forge build --push ghcr.io/acme

# deploy with context guard
forge deploy staging
```

## When this skill is not enough

- Production deploy → see `deploy` skill (`forge deploy <env>`)
- Greenfield setup → see `getting-started`
- Multi-cluster operator workflows → see `operators`
- Observability stack queries → see `observability`
