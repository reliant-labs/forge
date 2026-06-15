---
name: deploy
description: Ship code — lint, build, deploy to k3d/staging/prod, and verify.
---

# Ship It

`forge deploy <env>` dispatches per-service on the deploy target declared
in `deploy/kcl/<env>/main.k`. KCL is canonical: forge.yaml no longer
carries an `environments:` block, and there is no separate "deploy mode"
flag — the provider is whatever the service's `deploy = ...` resolves to:

| KCL deploy schema | Provider | Use for |
|---|---|---|
| `forge.K8sCluster` | k8s native (`cluster.Apply`) | Anything in a k8s cluster you control (k3d / GKE / EKS) |
| `forge.External` | generic shell-command | Fly.io, Cloud Run, Cloudflare Workers, ECS, Lambda, Vercel, Railway, systemd-on-VM — anything CLI-driven |
| `forge.Compose` | docker-compose | Docker-compose on a remote host |
| `forge.HostDeploy` | host process | Dev-loop only — `forge run` / `forge up` launches it locally |
| `forge.BuildOnly` | build, don't deploy | CLIs and library binaries with no runtime scheduling |

All four runtime providers honour `--dry-run` and `--rollback` (with the
caveat that `External` rollback requires `rollback_cmd` set in the KCL
block). See the `external-deploy-recipes` skill for copy-paste KCL for
the most common External targets.

The full shipping workflow: pre-flight checks, build, deploy, verify, rollback.

## Pre-flight checks

```
forge lint              # Go + proto + frontend linters (same checks CI runs)
forge lint --fix        # auto-fix where possible
forge lint --proto      # proto method enforcement
forge lint --contract   # contract interface enforcement
forge test              # full test suite must pass
```

## Build

```
forge build                     # binary + frontends
forge build --docker            # Docker images for all services
forge build --env=<env>         # filter to services NOT in host-only mode for that env
forge build --tag=<tag>         # override image tag (default: commit SHA)
forge build --target-arch=arm64 # cross-compile + buildx --platform override
forge build --debug             # with debug symbols for Delve
```

`forge build --env=<env>` reads `deploy/kcl/<env>/main.k` and skips any
service whose `deploy.type == "host"` — those services live on a
developer machine, not in an image. The same filter applies inside
`forge up --env=<env>`.

### Multi-source Docker builds (`docker.build_contexts`)

When a Dockerfile needs files from outside the project tree — a sibling
checkout the `go.mod` `replace`s against, a shared-libs monorepo
sibling, a base image you want to pin or override locally — declare the
extra contexts in `forge.yaml` and let `forge build --docker` (and the
deploy-time rebuild) pass them to `docker buildx` for you:

```yaml
docker:
  registry: ghcr.io/acme
  build_contexts:
    shared: ../shared-libs            # relative path, resolved against forge.yaml's dir
    sibling: ../../other-repo          # sibling checkout (e.g. cp-forge needs reliant code)
    base: docker-image://acme/base:v3  # registry image — pin or local-override a FROM
```

Consume them from the Dockerfile via `FROM <name>` or `COPY --from=<name>`:

```dockerfile
FROM base AS runtime
COPY --from=shared /go/pkg/mod/cache/ /go/pkg/mod/cache/
COPY --from=sibling /workspace/internal/ /workspace/sibling-internal/
```

Each entry becomes a `docker buildx --build-context name=value` arg.
Relative paths resolve against the project root; anything with a `://`
scheme (e.g. `docker-image://`, `oci-layout://`) passes through to
buildkit unchanged. No CLI flag — this is forge.yaml-only, since
the user already has `docker buildx --build-context` for ad-hoc cases.

## Deploy

Environment is a positional arg — `forge deploy dev`, not `forge deploy --env dev`.

```
forge deploy dev              # auto-detects provider per service (K8sCluster/External/Compose)
forge deploy staging          # staging environment
forge deploy prod             # production (must explicitly type "prod")
forge deploy dev --dry-run    # render/print without applying — works for every provider
forge deploy prod --rollback  # roll back to the previous good tag (mutually exclusive with --tag)
forge deploy dev --tag=<tag>  # override image tag (default: commit SHA)
```

`--dry-run` and `--rollback` are honoured across all four runtime
providers (K8sCluster, External, Compose, HostDeploy). For External,
`--rollback` reads the last good tag from
`.forge/state/external-<env>-<service>.json` and substitutes it into
`rollback_cmd`; deploys with no `rollback_cmd` declared error loudly
rather than guessing.

## forge up — full local-dev orchestrator

```
forge up --env=dev              # build + deploy + host launch + frontend dev — single command
forge up --env=dev --no-build   # skip the build phase
forge up --env=dev --no-deploy  # skip the cluster apply
forge up --env=dev --cluster-only  # only the in-cluster services
forge up --env=dev --host-only     # only the host-mode services
forge up --env=dev --background    # detach + return immediately
```

`forge up` reads `deploy/kcl/<env>/main.k` to split services by
provider, runs `forge build --env=<env>` (host services skipped),
applies cluster manifests via `internal/cluster.Apply`, launches host
services via `internal/hostlaunch`, then dev-serves every frontend.

`forge up` does NOT run `npm run build` for frontends — it `npm run
dev`s them so the dev server picks up source changes. The declared
`forge.Frontend.port` is force-injected as `PORT` into the Next.js
subprocess so the dev server binds the canonical port regardless of
whatever bled in from the parent env.

## Verify

After every deploy, confirm pods are healthy:

```
kubectl get pods -n <namespace>
kubectl logs -n <namespace> -l app=<service>
```

## Rollback

Fast revert with `kubectl rollout undo deployment/<name>`, then fix forward via KCL. Never leave a rollback as the permanent state.

## Rules

- Never skip lint — `.golangci.yml` and `buf.yaml` are the contract.
- Never `//nolint` without a reason comment: `//nolint:errcheck // best-effort cleanup`.
- Image tags must be immutable — commit SHA by default, never `:latest`.
- Secrets never live in KCL deploy files — they're checked in, treat as public.
- KCL schema changes are forever in production overlays — deprecate, don't delete.
- Don't `kubectl apply` hand-edited manifests — everything through `deploy/kcl/`.

## Per-env config rendering (`config_gen.k`)

`forge generate` emits `deploy/kcl/<env>/config_gen.k` from
`proto/config/v1/config.proto` + the sibling `config.<env>.yaml` file
next to forge.yaml. (Per-env config used to live in
`forge.yaml -> environments[].config`; that block was removed — see
the `environments-to-kcl` migration skill.) A few things are
surprising on first read:

- **Default-only fields don't appear.** If a proto field has a
  `default_value:` and no per-env override in `config.<env>.yaml`, the
  generated KCL skips the env-var entirely. The binary applies the
  default at startup via `pkg/config/config.go::Load()`. The rendered
  Deployment will NOT show the default as a literal env var, and that
  is intentional — defaults are the binary's contract, not the
  manifest's. To make a default visible in the rendered manifest, add
  the value (or a `${secret-ref}`) to `config.<env>.yaml`.
- **Empty categories are elided.** A category that ends up with no
  emitted entries (e.g. all of its fields are non-sensitive defaults)
  is skipped from the generated file rather than emitting an empty
  `<CATEGORY>_ENV: [schema.EnvVar] = []`. If your `main.k` references
  `cfg.<CATEGORY>_ENV` and KCL errors with "no attribute", regenerate
  and concatenate only the categories that actually produced entries.
- **Sensitive fields always emit.** Every `(forge.v1.config) = {
  sensitive: true }` field projects to a `secret_ref` EnvVar
  regardless of whether `config.<env>.yaml` provides a `${...}`
  override — the project-level default secret name + lowercased
  env-var key apply unconditionally.
- **Component config-block leaves use flat keys.** Fields of a
  component config block (`message TraderConfig { int32 max_per_tick
  ... }` composed on `AppConfig` — see the `architecture` skill,
  "Component config blocks") participate in `config.<env>.yaml` under
  their own snake_case leaf name (`max_per_tick: 50`), the same flat
  namespace as root fields, and project to the ConfigMap/env vars
  identically. Keep leaf names unique across blocks.

## Cross-references between schemas — declare once, denormalize at render

When two fields must agree — a service's bind port and the HTTPRoute that targets it, or a service's name and a route's backend ref — **declare the value on one schema and reference it from the other**. KCL expands the reference at render time, so the rendered output carries the literal value in both places, but the user-edited input has only one source of truth:

```kcl
ADMIN = forge.Service {
    name = "admin-server"
    port = 8090
    source = forge.GoSource { path = "handlers/admin_server" }
}

ADMIN_ROUTE = forge.HTTPRoute {
    host    = "admin.localhost"
    service = ADMIN.name      # cross-reference, not literal "admin-server"
    port    = ADMIN.port      # denormalized at render: both end up 8090
}
```

The rendered JSON:

```json
{
  "services":    [{"name": "admin-server", "port": 8090, ...}],
  "http_routes": [{"host": "admin.localhost", "service": "admin-server", "port": 8090}]
}
```

Both fields carry `8090` literally — consumers (`forge run`'s dev proxy, the cluster Gateway, audit/explain tools) read the denormalized output and trust it. Drift between the route and the service it targets is impossible by construction.

When to lean on cross-references:
- **Port** — any place a route, ingress, or sidecar declares the port of a service it points at.
- **Name** — backend `service =` fields, gateway `listener =` refs, anywhere one schema names another.
- **Image** — if two services need to share an image tag (e.g. a sidecar built from the same source), reference `MAIN.image`.
- **Per-env scaling toggles** — e.g. `dev = MAIN | { replicas = 1 }` overlays via the spread operator so only the env-specific field is rewritten.

The principle: **normalized KCL in, denormalized JSON out.** Consumers never read KCL syntax; they read the rendered bundle. Anything you can derive from one source belongs as a cross-reference, not a duplicated literal.

## Per-env conditional manifests via `option("env")`

The forge CLI passes the current env name to KCL as `-D env=<env>` on
every render invocation. Use `option("env")` in `main.k` to gate
fields per-env — typically `additional_manifests` for in-cluster infra
that should ship to k3d / staging / prod but not to a `dev-host` env
where docker-compose provides the same dependencies:

```kcl
_env = option("env")

_bundle = forge.Bundle {
    additional_manifests = [] if _env == "dev-host" else [
        # in-cluster NATS, Temporal, LiteLLM, etc.
    ]
}
```

## `features:` block — disabling subsystems

`forge.yaml` carries a `features:` block that gates `forge deploy`,
`forge build`, frontend codegen, packs, starters, CI, docs, and
observability. Defaults differ per `kind:`:

- `service` (default): every feature enabled.
- `cli`: build/ci/docs enabled; deploy/frontend/packs/starters/
  observability/codegen disabled.
- `library`: docs/contracts enabled; everything else disabled.

Disabled commands return `feature 'X' is disabled in forge.yaml.
Set features.X: true to enable.`. Disabled phases inside `forge up`
log a skip line and continue — `forge up` succeeds against whatever
subsystems are turned on.

## k3d local-registry mirror (`localhost:5050` ↔ `registry.localhost:5000`)

`forge deploy dev` builds and pushes images to host-side
`localhost:5050`. In-cluster pulls hit `registry.localhost:5000`. A
containerd mirror config bridges the two — without it, `docker push`
succeeds and pods `ImagePullBackOff` because `localhost:5050` doesn't
resolve from inside the node container.

Forge-managed k3d clusters get the mirror automatically:

- The project-templated `deploy/k3d.yaml` carries the mirrors inline
  (`registries.config` block of the k3d Simple config).
- The fallback `forge deploy dev` create path (no `deploy/k3d.yaml`)
  writes a temp `registries.yaml` and passes `--registry-config` to
  `k3d cluster create`.

**Pre-existing k3d cluster mirror fix.** If your cluster pre-dates this
behavior — `forge deploy dev` reuses the existing cluster, and pulls
fail with `localhost:5050: connection refused` from inside the node —
add the mirror to the running node container directly:

```bash
# Replace `k3d-dev-server-0` with your cluster's server node name
# (`docker ps --filter name=k3d`).
docker exec k3d-dev-server-0 sh -c 'cat > /etc/rancher/k3s/registries.yaml <<EOF
mirrors:
  "registry.localhost:5000":
    endpoint:
      - http://registry.localhost:5000
  "registry.localhost:5050":
    endpoint:
      - http://registry.localhost:5000
  "localhost:5050":
    endpoint:
      - http://registry.localhost:5000
EOF
'
docker restart k3d-dev-server-0
```

Or, simpler: `k3d cluster delete dev && forge deploy dev` recreates
the cluster with the mirror in place.
