---
name: dev
description: Local-cluster dev loop primitives — cluster lifecycle, status, logs, ingress URLs, host/cluster split. Compose with project-specific bash for sibling-repo deploys, helm bootstraps, and webhook listeners.
---

# Forge Dev Loop

For local-dev-against-a-cluster workflows. For local-go-only (no k8s) see the
`getting-started` skill.

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
| `forge env up --target <service> --host-only [--background]` | Host-mode single-service runner. Dispatches on the KCL `host.runner` (`go-run` / `air` / `binary` / `delve`), injecting the bundle `secret_provider` (dotenv in dev) as the secrets layer, then layering `env_vars` on top. For services whose dev env attaches a `host = forge.HostOverrides {...}` block. Skips build + cluster apply, scoped to the named service. |
| `forge env options <env> [--json]` | List the `-D` render options that env's KCL declares (see below). |
| `forge env down <env> [--all]` | Stop this project's stack for that env, tracked or orphaned. `--all`: all of them, machine-wide. |
| `forge env ps` | Every stack running here: project dir, env, process count. |
| `forge env up <env> [--no-build] [--no-deploy] [--cluster-only] [--host-only] [--target <name>] [-D name=value] [--background]` | The whole-loop orchestrator: build (host-mode services filtered out) → cluster apply → host launch → frontend dev-serve. Reads `deploy/kcl/<env>/` to split services by provider. |
| `forge env deploy dev [--prune] [--target <app>]` | Apply `deploy/kcl/dev/`. Skips rollout wait for services declaring `deploy = forge.HostDeploy {...}`. `--prune` deletes orphan forge-managed Deployments. `--target <app>` (repeatable, by service/frontend name) deploys ONLY that app — the K8sCluster apply keeps the app's workloads plus shared resources (Namespace, ConfigMap/Secret, RBAC) and drops other apps' workloads; a typo'd target errors with the available app names. |

## Host vs cluster: where does each service run in dev?

Default is **cluster**: every service runs in k3d, reached from the host
via the Gateway API ingress path (`forge cluster urls` lists the routes).
This is the right shape for services that need cluster-only primitives —
operators, CRD watchers, ingress webhooks, sidecars that depend on
dynamic-config injection.

**Host mode** flips a service to run as a host process under `forge env up
--target <service> --host-only`. Set the deploy target in
`deploy/kcl/<env>/main.k` to
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
    secret_provider = forge.DotenvSecrets { path = ".env.dev.secrets" }   # gitignored
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

The host/cluster split is a per-env concern. `forge env up staging` and
`forge env deploy prod` see whatever each env's `main.k` declares — typically
every service on `forge.K8sCluster` in staging / prod regardless of what
dev does.

`HostDeploy` env composition splits config from secrets:

- `env_vars` — KCL-declared per-env config (DATABASE_URL, NATS_URL, LOG_LEVEL,
  …). Reproducible, version-controlled; the same sources `K8sCluster` services
  see via the Deployment's env block.
- Secret VALUES come from the bundle-level `secret_provider`, NOT a per-service
  file. In dev that's `forge.DotenvSecrets { path = ".env.dev.secrets" }` — a
  gitignored dotenv with JUST the secrets (STRIPE_*, SUPABASE_*,
  JWT_PUBLIC_KEY, …), injected per runtime as the secrets layer FIRST;
  `env_vars` layers on top so KCL wins on conflict and per-env config can't
  drift between machines. A per-service `HostDeploy.secrets_file` is now only a
  backward-compat fallback. See the `forge/secrets` skill.

What flipping a service to host mode buys:

- `forge env deploy dev` skips its rollout wait (saves 120s/service).
- `forge env deploy dev --prune` deletes its stale in-cluster Deployment.
- `forge build dev` lists it under "host-mode services" so users know
  they need to run it with `forge env up --target <name> --host-only` (or just
  `forge env up dev`).
- The scaffolded `cmd/<bin>/cmd/serve.go` operator-gating helper won't start the
  controller manager when the user filters to host-mode-only services
  (no more spurious "not running in-cluster" errors during a host run).

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

After a batch of proto edits, `forge scaffold` catches the tree up: it
births every `// forge:entity`-marked message (CRUD quintet + owned migration),
then runs generate — which emits a pb-through handler stub for every custom RPC
(see `getting-started` and `db`). `forge run` (and `forge env up`) also
auto-seeds a fresh dev DB from the applied schema on first boot, so a clean
checkout comes up with FK-coherent demo data (dev only; `--no-seed` opts out,
`forge db seed status` inspects).

Use the breakdown below when you want fine-grained control:

```bash
# Terminal 1: long-running infra + cluster services
forge cluster up --wait
forge env deploy dev

# Terminal 2: the service you're actively editing
forge env up --target admin-server --host-only                 # foreground; Ctrl-C to stop
# or detach + tail logs separately:
forge env up --target admin-server --host-only --background    # detach; PIDs tracked per env
forge env down dev                                    # later teardown
```

The host child process also inherits the host shell's env, so anything already
exported wins over both the injected `secret_provider` layer and `env_vars`.

## Render options: varying a run without editing KCL

Some things you want to change per run, not per commit — which runner a host
service launches under, whether to point at a remote dependency. Those are
**render options**: your env's KCL declares them, `forge env up -D` sets them.

An env declares an option by *reading* it, so there is nothing to keep in sync
— the call site is the declaration:

```python
# deploy/kcl/dev/main.k
_host_runner = option("host_runner", type="str", default="air",
                      help="Host launch runner: air (default) or go-run")
```

```
forge env options dev            # what this env declares
forge env up dev -D host_runner=go-run
```

forge does **not** interpret the value. It checks the *name* against what the
env declares (a typo fails instead of silently doing nothing — an unread
option is not an error in KCL), relays the value verbatim as a string, and
your KCL decides what it means. That is the division: forge owns the
transport, your KCL owns the meaning.

Two consequences worth knowing:

- **Options forge derives are refused.** `env`, `namespace`, `image_tag`,
  `image_digests`, `worktree`, `branch` are computed by forge; a caller-supplied
  value would disagree with what was actually built and applied.
- **`-D` is accepted on `env up` only**, never `env deploy`. A cluster apply
  has to be reproducible from the repo alone.

Because the KCL resolves the option, it can do things a CLI flag could not.
The canonical case is leaving Air, which is not a one-field change — the Air
config is usually the only place a service's real entrypoint is written down,
and `HostOverrides` forbids `air_config` when the runner is not `air`:

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

Declare `type` / `default` / `help` on the `option()` call: they are optional,
but they are what `forge env options` has to show the next reader.

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

After the host + frontend phases start, `up` prints a summary box of what
is listening where and the log path for each process:

```
╭─ forge env up · env=dev ─────────────────────────────────────
│ Host services
│   admin-server           http://localhost:8080
│     ↳ .forge/logs/dev/admin-server.log
│ Frontends
│   reliant-web            http://localhost:3000
│     ↳ .forge/logs/dev/frontend_reliant-web.log
│
│ Logs   .forge/logs/dev/   — tail -f / grep the per-service *.log here
│ Cluster routes:  forge cluster urls
│ Ctrl-C to stop.
╰─────────────────────────────────────────────────────────
```

Host-service URLs are derived from each service's KCL `PORT` env var;
a service that declares no `PORT` is listed without a URL. Cluster
service routes (Gateway API) are not host-local — list them with
`forge cluster urls`.

## Composing with Taskfile (cloud-dev pattern)

The host/cluster split makes the canonical `task dev` shape:

```yaml
# Taskfile.yml — mirrors source's `make run-admin`
tasks:
  dev:
    desc: Bring up cluster + cluster services, run host services locally
    cmds:
      - forge cluster up --wait
      - forge env deploy dev --prune       # cluster services only; host services pruned
      - forge env up --target admin-server --host-only --background
      - forge env up --target workspace-proxy --host-only --background

  dev-stop:
    cmds:
      - forge env down dev
```

## Safety: kubectl context pinning

Every `forge cluster` command runs against `k3d-<cluster-name>` (resolved from
`deploy/k3d.yaml` metadata.name, falling back to forge.yaml `name`). This means
you cannot accidentally `forge cluster reload` into staging or prod.

`forge env deploy <env>` is DECLARATIVE-ONLY for cluster selection: the target
kubectl context comes SOLELY from the env's `forge.K8sCluster.cluster` in
`deploy/kcl/<env>/main.k`, threaded as `--context <declared>` on every kubectl
call. It never reads or falls back to your current kubectl context, and there is
no CLI override. For dev this defaults to `k3d-<project>`; for staging/prod
declare the expected context explicitly:

```kcl
# deploy/kcl/prod/main.k
import forge

_prod_k8s = forge.K8sCluster {
    cluster = "gke_acme-prod_us-central1_cluster-1"
    namespace = "myapp-prod"
}
```

The deploy fails fast (even under `--dry-run`) if the declared cluster has no
matching kubectl context. The only remedy is to fix your kubeconfig (e.g.
`gcloud container clusters get-credentials ...`) or correct
`forge.K8sCluster.cluster` in the env's KCL — there is no `--context` escape
hatch. Use `forge env deploy <env> --explain` to print the declared context and
whether it exists.

## When the project needs more

Forge owns the universal cluster + ingress + status mechanics; the project owns
project-specific orchestration (see "What forge does NOT own" below) in
`scripts/` and `Taskfile.yml`. Compose them:

```yaml
# Taskfile.yml
tasks:
  cloud-dev:
    desc: Full dev loop with sibling deploys
    cmds:
      - forge cluster up --wait
      - task deploy-reliant       # your bash — sibling-repo helm install
      - task ensure-litellm-db    # your bash — out-of-band DB bootstrap
      - forge env deploy dev          # forge KCL apply (with context guard)
      - task stripe-listen &
      - wait
```

## Multi-worktree / multi-namespace

`forge cluster instances` lists every dev namespace on the host — for
per-worktree namespacing (each worktree its own namespace, one shared cluster):

```bash
$ forge cluster instances
CLUSTER             NAMESPACE                           PODS
cp-forge            cp-forge-dev                        12
cp-forge            cp-forge-dev-feat-billing           12
cp-forge            cp-forge-dev-fix-auth               12
```

Each worktree sets the `namespace` field on its `forge.K8sCluster` block in
`deploy/kcl/dev/main.k` (or via the `FORGE_DEV_NAMESPACE` env override if your
bootstrap supports it), so worktrees run concurrently against one cluster.

## What forge does NOT own

- Sibling-repo deploys (project-specific helm installs, manifest applies)
- Helm chart bootstraps (project-specific stack — Postgres, Redis, observability)
- Webhook listeners (Stripe `stripe listen`, GitHub `gh webhook forward`, etc.)
- Project-specific DB seeding (custom schema + fixtures)
- Cross-service smoke tests (project-specific business invariants)

Keep these in `scripts/` and call them from `Taskfile.yml`. Compose them with
`forge cluster` primitives.

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
- Greenfield setup → see `getting-started`
- Multi-cluster operator workflows → see `operators`
- Observability stack queries → see `observability`
