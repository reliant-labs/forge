---
name: dev
description: Local-cluster dev loop primitives — cluster lifecycle, port-forward, status, logs. Compose with project-specific bash for sibling-repo deploys, helm bootstraps, and webhook listeners.
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

## Safety: kubectl context pinning

Every `forge dev` command runs against `k3d-<cluster-name>` (resolved from
`deploy/k3d.yaml` metadata.name, falling back to forge.yaml `name`). This means
you cannot accidentally `forge dev cluster reload` into staging or prod.

`forge deploy <env>` enforces the same guard: before applying, it verifies the
current kubectl context matches `environments[<env>].cluster` from forge.yaml.
For dev this defaults to `k3d-<project>`; for staging/prod declare the expected
context explicitly:

```yaml
# forge.yaml
environments:
  - name: prod
    cluster: gke_acme-prod_us-central1_cluster-1
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

Each worktree sets `environments[].namespace` in its branch's forge.yaml (or
via the `FORGE_DEV_NAMESPACE` env override if supported by your bootstrap)
so multiple worktrees can run concurrently against one shared cluster.

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
