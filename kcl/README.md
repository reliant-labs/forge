# forge — KCL module

Typed schemas + manifest render layer that forge projects import.

```kcl
import forge

# The env-wide Kubernetes facts, stated ONCE. Carried on the Bundle's
# `cluster_target`; each service references its derived `.deploy` rather
# than restating cluster/namespace/registry per service.
_k8s = forge.ClusterTarget {
    cluster = "k3d-myapp"
    namespace = "myapp-dev"
    registry = "localhost:5050"
}

forge.Service {
    name = "admin-server"
    image = "myapp:dev"
    deploy = forge.HostDeploy { runner = "air", air_config = ".air.toml" }
}

forge.Service {
    name = "workspace-proxy"
    image = "myapp:dev"
    # Reference the env-wide target; overlay only the per-service knobs.
    deploy = _k8s.deploy | { replicas = 1, ports = [8080] }
}

forge.Operator {
    name = "workspace-controller"
    image = "myapp:dev"
    crds = ["Workspace"]
    cluster_rbac = forge.ClusterRBAC {
        rules = [{ apiGroups = ["forge.io"], resources = ["workspaces"], verbs = ["*"] }]
    }
}

forge.Frontend {
    name = "admin-web"
    path = "frontends/admin-web"
}

forge.CronJob {
    name = "billing-sweep"
    schedule = "@hourly"
    image = "myapp:prod"
    command = ["./myapp", "cron", "billing-sweep"]
}
```

## What ships here

Four typed entity schemas — each captures ONE orchestration shape so the
forge CLI can dispatch on intent rather than infer it:

| Schema | Purpose | JSON bucket |
|--------|---------|-------------|
| `Service`  | Long-running server (RPC / HTTP). Host or in-cluster. | `services[]`  |
| `Operator` | Cluster-scoped controller that reconciles CRDs.       | `operators[]` |
| `Frontend` | Web or mobile frontend (Next.js / Vite / RN).         | `frontends[]` |
| `CronJob`  | Scheduled job. Omit `schedule` → renders a Job.       | `cronjobs[]`  |

`Service.deploy` is a polymorphic union — one of `HostDeploy`,
`K8sCluster`, `External`, `Compose`, or `BuildOnly`. The `type`
discriminator lives ON the deploy subschema so KCL's JSON output is
self-describing. Forge's CLI reads the discriminator to decide whether
to run on host, schedule in cluster, shell out to a custom CLI, or
just produce a build artifact.

`External` is the escape hatch for any deploy target driven by a CLI
(Fly.io / Cloudflare Workers / Cloud Run / ECS / Vercel / systemd VM
/ …). The provider exec's `deploy_cmd` with substitution tokens
(`${IMAGE}`, `${TAG}`, `${LAST_TAG}`, `${SERVICE}`, `${ENV}`,
`${ENV_FILE}`, `${PROJECT_DIR}`, plus any keys declared in `env`).
See the `external-deploy-recipes` skill for ready-to-paste KCL blocks
for the common providers.

`HostDeploy` splits per-env config from secrets:

| Field          | Source              | Reproducible? |
|----------------|---------------------|---------------|
| `env_vars`     | KCL (this file)     | Yes — version-controlled |
| `secrets_file` | gitignored dotenv   | No — per developer |

Forge's `forge up` host phase loads `secrets_file` first
(if set), then layers `env_vars` on top so KCL-declared config wins on
conflict. Host services see the same per-env config source that
`K8sCluster` services see via the Deployment's `env` block — the split
keeps host and cluster from drifting.

`CLI` / `Job` collapse:
- A CLI tool is a `Service` with `deploy = forge.BuildOnly{...}`.
- A one-shot Job is a `CronJob` with `schedule = ""` (renders as a Job
  instead of a CronJob).

`Operator` stays separate even though it could fit `Service` because
its intent (reconcile CRDs, needs cluster-scoped RBAC, no host story)
is meaningfully different and the JSON consumer benefits from a
typed bucket.

## How projects consume this

Project's `deploy/kcl/kcl.mod`:

```toml
[package]
name = "myapp"
edition = "v0.11.0"
version = "0.0.1"

[dependencies]
forge = { git = "https://github.com/reliant-labs/forge.git", tag = "kcl-v0.1.0" }
```

Project's `deploy/kcl/dev/main.k`:

```kcl
import forge

entities = forge.Bundle {
    services = [
        forge.Service { name = "admin-server", image = "myapp:dev",
                        deploy = forge.HostDeploy { runner = "air" } }
    ]
    operators = []
    frontends = [
        forge.Frontend { name = "admin-web", path = "frontends/admin-web" }
    ]
    cronjobs = []
}

# Render the JSON contract that forge build/run/deploy consumes.
output = forge.render(entities)
```

Then:

```bash
kcl run deploy/kcl/dev/ -S output --format json
```

## Standard `-D` render options

The forge CLI drives every render with a standard set of top-level KCL
bindings (`kcl run -D <key>=<value>`). They carry per-invocation facts into
your `main.k`. Read them through the **typed `forge` accessors** (each wraps
`option(...)` with a default + doc) rather than raw `option()` so the whole
set is discoverable from the `forge` surface:

| `-D` key        | Accessor                  | Always passed? | What it is |
| --------------- | ------------------------- | -------------- | ---------- |
| `env`           | `forge.env(default)`      | yes            | environment name (`dev`/`staging`/`prod`/…) |
| `image_tag`     | `forge.image_tag(env)`    | yes            | resolved image tag (override > per-env default > `latest`) |
| `namespace`     | `forge.namespace(default)`| yes            | k8s namespace to deploy into |
| `image_digests` | `forge.image_digests()`   | when deploying | JSON name→digest map (pins each image to its digest) |
| `registry`      | `forge.registry(default)` | no (override)  | image registry; the per-env literal is yours, `-D registry=` overrides it |

Plus every non-sensitive per-env **config** field is passed as its own
`-D <FIELD>=` and surfaced through the generated `config_gen.k`
(`cfg.APP_ENV` / `cfg.CONFIG_MAPS`) — read those via `cfg.<...>`, never raw
`option()`.

### Per-env conditional manifests

Use `forge.env()` to conditionally include manifests — e.g. skip in-cluster
infra on `dev-host` envs where docker-compose already provides those
services:

```kcl
_is_dev_host = forge.env() == "dev-host"

_bundle = forge.Bundle {
    services = [...]
    additional_manifests = [] if _is_dev_host else [
        # in-cluster NATS, Temporal, LiteLLM, etc.
    ]
}
```

## Secrets — `${NAME#KEY}` config-contract override

For a SENSITIVE config field (`sensitive: true` in the config proto), forge's
config codegen emits the `secret_ref` EnvVar for you into
`deploy/kcl/<env>/config_gen.k`, defaulting to the `<project>-secrets` Secret
and a `<env_var lowercased>` key. To bind a field to a DIFFERENT existing
cluster Secret/key, set its value in the per-env `config.<env>.yaml` to a
reference string:

```yaml
# config.prod.yaml
#   "${NAME}"      -> secret_ref = NAME            (key = codegen default)
#   "${NAME#KEY}"  -> secret_ref = NAME, key = KEY (override both)
internal_service_secret: "${control-plane-internal#secret}"
```

The `#KEY` form is for cluster secrets whose keys are kebab-case (e.g.
`"${db-credentials#database-url}"`) and don't match forge's
lowercase-env-var default. The Secret itself is provisioned out-of-band
(ESO / sealed-secrets / `kubectl create secret`). Same rule on a hand-written
`forge.EnvVar` — see the `EnvVar` schema doc in `schema.k`.

## Versioning

Pin a `kcl-vX.Y.Z` git tag from your project's `kcl.mod`. The forge CLI
ships migrations per tagged release so `forge upgrade` can bump
versions deterministically. See `migration/v0.x-to-kcl-module/SKILL.md`.

## Layout

```
kcl/
  kcl.mod              # module declaration (this file)
  README.md            # you are here
  schema.k             # all typed schemas (Service / Operator / Frontend / CronJob, deploy union)
  base.k               # shared helpers (env vars, init containers)
  render.k             # entities → JSON contract + k8s manifests
  lib/
    services.k         # service-specific manifest builders
    crd.k              # CRD builders
    rbac.k             # RBAC builders (namespaced + cluster)
    netpol.k           # NetworkPolicy builders
  example/             # tiny example project consumed by tests
    dev/main.k
  tests/               # KCL-level invariant tests (`kcl run tests/*.k`)
```
