---
name: deploy
description: Ship code — lint, build, deploy to k3d/staging/prod, and verify.
---

# Ship It

`forge env deploy <env>` dispatches per-service on the deploy target declared
in `deploy/kcl/<env>/main.k`. KCL is canonical: there is no forge.yaml
`environments:` block and no separate "deploy mode" flag — the provider is
whatever the service's `deploy = ...` resolves to:

| KCL deploy schema | Provider | Use for |
|---|---|---|
| `forge.K8sCluster` | k8s native (`cluster.Apply`) | Anything in a k8s cluster you control (k3d / GKE / EKS) |
| `forge.External` | generic shell-command | Fly.io, Cloud Run, Cloudflare Workers, ECS, Lambda, Vercel, Railway, systemd-on-VM — anything CLI-driven |
| `forge.Compose` | docker-compose | Docker-compose on a remote host |
| `forge.HostDeploy` | host process | Dev-loop only — `forge env up dev` launches it locally |
| `forge.BuildOnly` | build, don't deploy | CLIs and library binaries with no runtime scheduling |

See the `external-deploy-recipes` skill for copy-paste KCL for the most
common External targets.

## Pre-flight checks

```
forge lint              # Go + proto + frontend linters (same checks CI runs);
                        # deterministic-safe auto-fixes are applied by default
forge lint --no-fix     # gate only, mutate nothing (CI / read-only)
forge lint --conventions # proto convention rules
forge lint --contract   # contract interface enforcement
task test              # full test suite must pass
```

## Build

```
forge build                     # binary + frontends
forge build --docker            # Docker images for all services
forge build <env>         # filter to services NOT in host-only mode for that env
forge build --tag=<tag>         # override image tag (default: commit SHA)
forge build --target-arch=arm64 # cross-compile + buildx --platform override
forge build --debug             # with debug symbols for Delve
```

`forge build <env>` reads `deploy/kcl/<env>/main.k` and skips any
service whose `deploy.type == "host"` — those run on a developer
machine, not in an image. `forge env up <env>` applies the same filter.

### Multi-source Docker builds (`docker.build_contexts`)

When a Dockerfile needs files from outside the project tree (a sibling
checkout the `go.mod` `replace`s against, a shared-libs monorepo
sibling, a base image to pin or override locally), declare the extra
contexts in `forge.yaml`; `forge build --docker` and the deploy-time
rebuild pass them to `docker buildx`:

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
buildkit unchanged. No CLI flag — forge.yaml only.

## Deploy

Environment is a positional arg — `forge env deploy dev`, not `forge env deploy --env dev`.

```
forge env deploy dev              # auto-detects provider per service (K8sCluster/External/Compose)
forge env deploy staging          # staging environment
forge env deploy prod             # production (must explicitly type "prod")
forge env deploy dev --dry-run    # render/print without applying — works for every provider
forge env deploy prod --rollback  # roll back to the previous good tag (mutually exclusive with --tag)
forge env deploy dev --tag=<tag>  # override image tag (default: commit SHA)
```

`--dry-run` and `--rollback` are honoured across all four runtime
providers (K8sCluster, External, Compose, HostDeploy). For External,
`--rollback` reads the last good tag from
`.forge/state/external-<env>-<service>.json` and substitutes it into
`rollback_cmd`; deploys with no `rollback_cmd` declared error loudly
rather than guessing.

## External / host-VM targets (Fly, Cloud Run, scp-to-a-VM, systemd)

Declared per-env in `deploy/kcl/<env>/main.k`:

```kcl
MAIN = forge.Service {
    name   = "trader"
    image  = "registry.fly.io/trader"     # ${IMAGE} is hoisted from here
    deploy = forge.External {
        deploy_cmd   = "flyctl deploy -i ${IMAGE}:${TAG} -a trader"
        rollback_cmd = "flyctl deploy -i ${IMAGE}:${LAST_TAG} -a trader"  # optional
        health_cmd   = "curl -fsS https://trader.fly.dev/healthz"          # optional
        env_file     = "~/.config/trader/.env"                            # optional
        env          = { REGION = "iad" }                                  # optional map
    }
}
```

Only `deploy_cmd` is required.

**Substitution tokens.** forge expands these into `deploy_cmd` /
`rollback_cmd` / `health_cmd` (`${X}` and `$X`; unknown keys → empty
string):

| Token | Value |
|---|---|
| `${IMAGE}` | forge-built image name, hoisted from the surrounding `Service.image` |
| `${TAG}` | resolved tag (build-state or `--tag`) |
| `${CODE_VERSION}` | == `${TAG}` — pass to `docker run -e CODE_VERSION=…` / a label so the binary's reported `code_version` matches the image |
| `${PIPELINE}` | `"forge"` — label the container with it to distinguish forge deploys from manual ones |
| `${LAST_TAG}` | prior deployed tag (rollback target on rollback; empty on first deploy) |
| `${SERVICE}` | `Service.name` |
| `${ENV}` | env name (dev/staging/prod) |
| `${ENV_FILE}` | the `env_file` path (if any) |
| `${PROJECT_DIR}` | project root |
| any key in `env` | its declared value (built-ins win on conflict) |

**Build → deploy value flow.** A `forge build` writes a `BuildState`
(`image`, `tag`, `pushed`, …) to `.forge/state/build-<env>.json`;
`forge env deploy <env>` (no `--tag`) reads it and resolves `${TAG}` /
`${IMAGE}` from there, so the deploy reuses the exact tag the build
produced.

### Recommended pattern: single-image host-VM deploy (scp-to-VM)

One project image shipped to a VM — use forge's **native `--docker`
build**, not a custom `build_cmd`:

```bash
forge build --docker --tag v42          # local <registry>/<name>:v42, NOT pushed
```

`forge build --docker` builds the project's root `Dockerfile` into a
LOCAL `<registry>/<name>:<tag>` image (registry from forge.yaml's
`docker.registry`, falling back to the project name) and
AUTO-injects `--build-arg FORGE_VERSION/COMMIT/DATE`, so `code_version`
stamps correctly. It does NOT push unless you pass `--push <registry>`.

Then a custom `deploy_cmd` ships that local image — no `build_cmd`:

```kcl
deploy = forge.External {
    deploy_cmd = "docker save ${IMAGE}:${TAG} | ssh vm 'docker load && docker run -d --rm -e CODE_VERSION=${CODE_VERSION} ${IMAGE}:${TAG}'"
}
```

**When you DO need `build_cmd`:** only for genuine external/sibling-repo
builds (the "cp-forge" pattern, where a sibling binary is built by a
custom command). A custom `build_cmd` does NOT get forge's auto
version-injection — it must forward `${CODE_VERSION}` itself as a
build-arg, or `code_version` stamps `dev` — `build_cmd` owns BOTH the
build AND any push.

## forge env up — full local-dev orchestrator

```
forge env up dev              # build + deploy + host launch + frontend dev — single command
forge env up dev --no-build   # skip the build phase
forge env up dev --no-deploy  # skip the cluster apply
forge env up dev --cluster-only  # only the in-cluster services
forge env up dev --host-only     # only the host-mode services
forge env up dev --background    # detach + return immediately
forge env up dev -D host_runner=go-run   # set a render option the env declares
```

`-D` sets a **render option** the env's KCL declares by reading it
(`option("host_runner", type="str", default="air", help="...")`). forge
validates the name against what the env declares and relays the value
verbatim — it never interprets it. `forge env options <env>` lists them. Not
accepted on `env deploy`: a cluster apply must render from the repo alone.
See the `dev` skill for the full pattern.

It reads `deploy/kcl/<env>/main.k` to split services by provider, runs
`forge build <env>` (host services skipped), applies cluster manifests
via `internal/cluster.Apply`, launches host services via
`internal/hostlaunch`, then dev-serves every frontend.

It does NOT run `npm run build` for frontends — it `npm run dev`s them
so the dev server picks up source changes. The declared
`forge.Frontend.port` is force-injected as `PORT` into the Next.js
subprocess so the dev server binds the canonical port regardless of
whatever bled in from the parent env.

## Verify + rollback

After every deploy confirm pods are healthy (`kubectl get pods -n <ns>`,
`kubectl logs -n <ns> -l app=<service>`). Fast revert with `kubectl rollout
undo deployment/<name>`, then fix forward via KCL — never leave a rollback
as the permanent state.

## Rules

- Never skip lint — `.golangci.yml` and `buf.yaml` are the contract.
- Never `//nolint` without a reason comment: `//nolint:errcheck // best-effort cleanup`.
- Image tags must be immutable — commit SHA by default, never `:latest`.
- Secrets never live in KCL deploy files — they're checked in, treat as public.
- KCL schema changes are forever in production overlays — deprecate, don't delete.
- Don't `kubectl apply` hand-edited manifests — everything through `deploy/kcl/`.

## Per-env config — KCL is the surface

Per-env config (logging, env vars) lives **directly in
`deploy/kcl/<env>/`**. Set env vars on the `Service` and let the binary
apply proto defaults at startup via `internal/config` — don't project a
second redundant YAML into KCL.

```kcl
MAIN = forge.Service {
    name = "trader"
    port = 8090
    env  = {
        LOG_LEVEL    = "info"
        MAX_PER_TICK = "50"
    }
}
```

- **Defaults are the binary's contract, not the manifest's.** A proto
  field with a `default_value:` and no KCL override emits no env var;
  `internal/config` applies the default at startup. To make a default
  visible in the rendered Deployment, set it explicitly in the env's KCL.
- **Sensitive fields use secret refs, never literals.** A `(forge.v1.config)
  = { sensitive: true }` field is bound via a `${secret-ref}` per the
  `secrets` skill — its value never lands in rendered KCL.
- **Don't reach for a sibling `config.<env>.yaml`** — those files only
  shaped generated KCL projection, which is removed. Edit the env's KCL.
- **Component config-block leaves are flat env keys.** Fields of a
  component config block (`message TraderConfig { int32 max_per_tick
  ... }` composed on `AppConfig` — see `architecture`, "Component config
  blocks") are consumed as one typed `Cfg config.TraderConfig` Deps
  field and bound from the matching snake_case env var
  (`MAX_PER_TICK`). Keep leaf names unique across blocks.

## Cross-references between schemas — declare once, denormalize at render

When two fields must agree — a service's bind port and the HTTPRoute that targets it, a service's name and a route's backend ref — **declare the value on one schema and reference it from the other**. KCL expands the reference at render time: the rendered output carries the literal in both places, the user-edited input has one source of truth.

```kcl
ADMIN = forge.Service {
    name = "admin-server"
    port = 8090
    source = forge.GoSource { path = "internal/admin_server" }
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

**Normalized KCL in, denormalized JSON out.** Consumers (`forge env up`'s dev proxy, the cluster Gateway, audit/explain tools) read the rendered bundle, never KCL syntax — drift is impossible by construction. Lean on cross-references for:
- **Port** — any route, ingress, or sidecar declaring the port of a service it points at.
- **Name** — backend `service =` fields, gateway `listener =` refs, anywhere one schema names another.
- **Image** — two services sharing an image tag (e.g. a sidecar built from the same source) reference `MAIN.image`.
- **Per-env scaling toggles** — `dev = MAIN | { replicas = 1 }` overlays via the spread operator so only the env-specific field is rewritten.

## Per-env conditional manifests via `option("env")`

The forge CLI passes the env name to KCL as `-D env=<env>` on every
render. Use `option("env")` in `main.k` to gate fields per-env —
typically `additional_manifests` for in-cluster infra that ships to
k3d/staging/prod but not to a `dev-host` env where docker-compose
provides the same dependencies:

```kcl
_env = option("env")

_bundle = forge.Bundle {
    additional_manifests = [] if _env == "dev-host" else [
        # in-cluster NATS, Temporal, LiteLLM, etc.
    ]
}
```

## `features:` block — disabling subsystems

`forge.yaml`'s `features:` block gates `deploy`, `build`, `ci`, `codegen`,
`orm`, `migrations`, `frontend`, `observability`, `hot_reload`, `contracts`,
`docs` — plus experimental `ingress`, `external_builds`, `operators`,
`strict_wiring` under `features.experimental:` (always default-off). Each
value defaults from the project's DERIVED shape (`forge project audit`
prints it as `project_kind`); there is no `kind:` key to set:

- `service`: all on, minus `orm`/`migrations` (need `database.driver`) and
  `frontend` (needs a declared frontend).
- `cli`: `build`/`ci`/`contracts`/`docs` on; rest off.
- `library`: `contracts`/`docs` on; rest off.

An explicit `features.<name>: true|false` wins.

Disabled commands return `feature 'X' is disabled in forge.yaml.
Set features.X: true to enable.`. Disabled phases inside `forge env up`
log a skip line and continue — `forge env up` succeeds against whatever
subsystems are turned on.

## k3d local-registry mirror

Load the `deploy/k3d-registry` skill for the `localhost:5050` ↔
`registry.localhost:5000` containerd mirror — what forge configures
automatically, and the fix when a pre-existing cluster `ImagePullBackOff`s.
