# forge — KCL module

Typed schemas + manifest render layer that forge projects import.

```kcl
import forge

# The env-wide Kubernetes facts, stated ONCE. Carried on the Bundle's
# `cluster_target`, so no workload restates cluster/namespace/registry.
_k8s = forge.ClusterTarget {
    cluster = "k3d-myapp"
    namespace = "myapp-dev"
    registry = "localhost:5050"
}

# Target-agnostic workloads. There is NO `deploy` field: the presence of a
# `host` block routes this one through the host adapter, and its absence
# means k8s. One builder can therefore serve both the host dev env and the
# cluster envs from a single definition.
forge.Service {
    name = "admin-server"
    image = "myapp:dev"
    host = forge.HostOverrides { runner = "air", air_config = ".air.toml" }
}

forge.Service {
    name = "workspace-proxy"
    image = "myapp:dev"
    ports = [8080]
    replicas = 1
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

# A frontend whose code lives in ANOTHER repository, pinned to a ref —
# builds in CI (where only this repo is checked out) and builds the same
# bytes on every machine. See "Cross-repo sources" below.
forge.Frontend {
    name = "reliant-web"
    type = "vite"
    source = forge.GitSource {
        repo = "github.com/reliant-labs/reliant"
        ref = "v1.6.3"
        subdir = "web"
    }
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

| Schema     | Purpose                                               | JSON bucket   |
| ---------- | ----------------------------------------------------- | ------------- |
| `Service`  | Long-running server (RPC / HTTP). Host or in-cluster. | `services[]`  |
| `Operator` | Cluster-scoped controller that reconciles CRDs.       | `operators[]` |
| `Frontend` | Web or mobile frontend (Next.js / Vite / RN).         | `frontends[]` |
| `CronJob`  | Scheduled job. Omit `schedule` → renders a Job.       | `cronjobs[]`  |

### Cross-repo sources

A `Frontend` declares its code EITHER as a `path` (a directory in this
repo) OR as a `source = forge.GitSource { repo, ref, subdir? }` — never
both. The `source` form exists because a filesystem path to a sibling
checkout has two failure modes: it does not exist in CI, and where it does
exist it silently ships whatever happened to be checked out, so identical
commits produce different artifacts on different machines.

`ref` is required — forge does not default to a repository's default
branch. Fetches are cached per repo+ref, and a machine-local
`.forge/source-overrides.yaml` (gitignored, so it can never un-pin CI)
maps a repo to a working copy for local iteration.

See `docs/cross-repo-sources.md` for the full model.

### Two `Service` schemas: which one you write

`Service` (in `core.k`) is **the one you author** — target-agnostic, with
zero k8s vocabulary. Put these in `Bundle.workloads`. It has **no `deploy`
field**: target selection is _structural_, by which override block is
present.

| You write       | Renders to                                           |
| --------------- | ---------------------------------------------------- |
| a `host` block  | the host adapter (`go-run` / `air` / binary / delve) |
| no `host` block | k8s (Deployment + Service), the default              |
| a `k8s` block   | k8s, plus that escape hatch's overrides              |

A host-targeted `Service` that also sets k8s-only fields fails at KCL load
rather than silently dropping them.

`RenderedWorkload` (in `schema.k`) is the **k8s-shaped projection carrier**
the adapters produce. It is what `Bundle.services` holds, and it is
adapter-internal — you generally do not hand-author it. It carries the
polymorphic `deploy` union (`HostDeploy` / `K8sCluster` / `External` /
`Compose` / `BuildOnly`) whose `type` discriminator makes the JSON output
self-describing, so forge's CLI can tell whether to run on host, schedule in
cluster, shell out to a custom CLI, or just produce a build artifact.

Both are supported today: the agnostic `Service` is the path forward for
host and k8s, while compose, external and build-only targets still flow
through `RenderedWorkload` until they are folded into the core.

`External` is the escape hatch for any deploy target driven by a CLI
(Fly.io / Cloudflare Workers / Cloud Run / ECS / Vercel / systemd VM
/ …). The provider exec's `deploy_cmd` with substitution tokens
(`${IMAGE}`, `${TAG}`, `${LAST_TAG}`, `${SERVICE}`, `${ENV}`,
`${ENV_FILE}`, `${PROJECT_DIR}`, plus any keys declared in `env`).
See the `external-deploy-recipes` skill for ready-to-paste KCL blocks
for the common providers.

`HostDeploy` splits per-env config from secrets:

| Field          | Source            | Reproducible?            |
| -------------- | ----------------- | ------------------------ |
| `env_vars`     | KCL (this file)   | Yes — version-controlled |
| `secrets_file` | gitignored dotenv | No — per developer       |

Forge's `forge env up` host phase loads `secrets_file` first
(if set), then layers `env_vars` on top so KCL-declared config wins on
conflict. Host services see the same per-env config source that
`K8sCluster` services see via the Deployment's `env` block — the split
keeps host and cluster from drifting.

`CLI` / `Job` collapse:

- A CLI tool is a workload whose projection carries `deploy =
forge.BuildOnly{...}` — build the artifact, deploy nothing.
- A one-shot Job is a `CronJob` with `schedule = ""` (renders as a Job
  instead of a CronJob).

`Operator` stays separate even though it could fit `Service` because
its intent (reconcile CRDs, needs cluster-scoped RBAC, no host story)
is meaningfully different and the JSON consumer benefits from a
typed bucket.

## Extending a typed entity — `schema MyService(forge.Service)`

A project can use KCL-native inheritance to add its OWN typed/required
fields to a forge entity while forge renders the result EXACTLY like the
base entity:

```kcl
import forge

schema BillingService(forge.Service):
    region: str               # extra REQUIRED field — enforced at parse time
    tier: "free" | "pro" = "free"

    check:
        region, "BillingService.region is required"

_svc = BillingService {
    name = "billing-api", image = "billing-api", region = "us-east-1"
    ports = [8080]
}
```

This works because the render layer's lambdas are typed on the BASE
schema (`lambda s: Service -> ...`), which accepts any subtype: the
subtype passes through `forge.render` / `forge.render_manifests` and
projects the same JSON contract + k8s manifests a plain `forge.Service`
would. The app's extra fields ride on the typed value but are NOT part of
the rendered contract (they're yours, for your own KCL logic). An extra
field with no default — or a `check:` the value violates — fails at KCL
load, so your domain invariants are enforced the same way forge's are.

## Declared external prerequisites — `required_secrets` / `required_dns`

A deploy often depends on out-of-band facts forge does NOT (and must not)
create: the cert-manager `cloudflare-api-token` Secret, per-host DNS
A-records, the load-bearing `*.workspaces` wildcard. Left in a docstring,
`forge env deploy` renders green and THEN ACME / DNS hangs silently. Declare
them as first-class prerequisites on the Bundle so they're MODELED:

```kcl
_bundle = forge.Bundle {
    # ... services / gateways / ...
    required_secrets = [
        forge.ExternalSecret {
            name = "cloudflare-api-token"
            namespace = "cert-manager"      # often NOT the deploy namespace
            keys = ["api-token"]
            reason = "cert-manager DNS-01 Cloudflare API token"
        }
    ]
    required_dns = [
        forge.DNSRecord {
            host = "*.workspaces.example.com"
            reason = "DNS-01 wildcard cert + workspace-proxy traffic"
        }
    ]
}
```

What this buys (beyond a comment):

- **Render-time checklist** — `forge env deploy` prints the prerequisites
  every run; `forge project audit` surfaces them as the `prerequisites` category.
- **Deploy preflight BLOCK** — a declared `ExternalSecret` that's absent
  (or missing a declared key) on the live target FAILS the deploy before
  the first apply, in its OWN declared namespace, reusing the same
  SecretGetter the `secretKeyRef` preflight uses. DNS can't be verified
  authoritatively, so `required_dns` is a checklist note, not a block.
- **Cross-secret byte-match** — when ONE logical value is projected to N
  refs (the same token under two names), give each `ExternalSecret` a
  shared `value_group`. KCL rejects a group whose members declare
  different key sets at load; the preflight byte-compares the live values
  and BLOCKS on a divergence (a half-rotated credential).

`forge` never creates these resources — the declaration drives the
checklist + preflight only; nothing leaks into the rendered manifests.

## How projects consume this

Project's `deploy/kcl/kcl.mod`:

```toml
[package]
name = "myapp"
edition = "v0.11.0"
version = "0.0.1"

[dependencies]
forge = { path = "../../.forge-kcl" }
```

You do not write that dependency by hand, and there is no tag or registry to
pin. `forge generate` materializes this module — the copy embedded in the forge
binary — into `.forge-kcl/` at the project root and maintains the dependency
line above. The vendored copy travels with the repo, so containers, CI
checkouts and other machines resolve the identical module with no network and
no git auth. Commit `.forge-kcl/`.

Project's `deploy/kcl/dev/main.k`:

```kcl
import forge

entities = forge.Bundle {
    # `workloads` is the agnostic authoring entry. (`services` still accepts
    # pre-projected RenderedWorkloads for the targets not yet folded in.)
    workloads = [
        forge.Service {
            name = "admin-server"
            image = "myapp:dev"
            host = forge.HostOverrides { runner = "air" }
        }
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

| `-D` key        | Accessor                   | Always passed? | What it is                                                                |
| --------------- | -------------------------- | -------------- | ------------------------------------------------------------------------- |
| `env`           | `forge.env(default)`       | yes            | environment name (`dev`/`staging`/`prod`/…)                               |
| `image_tag`     | `forge.image_tag(env)`     | yes            | resolved image tag (override > per-env default > `latest`)                |
| `namespace`     | `forge.namespace(default)` | yes            | k8s namespace to deploy into                                              |
| `image_digests` | `forge.image_digests()`    | when deploying | JSON name→digest map (pins each image to its digest)                      |
| `registry`      | `forge.registry(default)`  | no (override)  | image registry; the per-env literal is yours, `-D registry=` overrides it |

Per-env **config** is NOT passed via `-D`: it lives in the typed `AppConfig`
instance in `deploy/kcl/<env>/config.k` and is projected into each workload's
env by `config_gen.appConfigEnvMap` — read config there, never raw
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

## Secrets — the `ConfigSecretRef` config-contract override

For a SENSITIVE config field (`sensitive: true` in the config proto), forge's
config codegen types the field as a `ConfigSecretRef` on the generated
`AppConfig` schema, defaulting to the `<project>-secrets` Secret and a
`<env_var lowercased>` key; `config_gen.appConfigEnvMap` projects it to a
`from_secret` EnvSource. To bind a field to a DIFFERENT existing cluster
Secret/key, set its `ConfigSecretRef` in the per-env `deploy/kcl/<env>/config.k`:

```kcl
# deploy/kcl/prod/config.k
app_config: config_gen.AppConfig = {
    internal_service_secret = config_gen.ConfigSecretRef {
        name = "control-plane-internal", key = "secret"
    }
}
```

Use a kebab-case `key` for cluster secrets whose keys don't match forge's
lowercase-env-var default (e.g. `key = "database-url"`). The Secret itself is
provisioned out-of-band (ESO / sealed-secrets / `kubectl create secret`). Same
`secret_ref`/`secret_key` fields exist on a hand-written `forge.EnvVar` — see
the `EnvVar` schema doc in `schema.k`.

## Versioning

The module version is the forge version that materialized it. There is no
separate tag to pin: `forge generate` refreshes `.forge-kcl/` from the running
binary and stamps that version into `.forge-kcl/.forge-version`.

Because the module refreshes only on `forge generate`, a project can sit on a
copy an older forge wrote. Forge notices: every render compares the stamp
against the running binary and warns when they differ, naming `forge generate`
as the fix.

When a release changes this module's schemas in a breaking way, forge ships a
migration skill for that release; `forge project upgrade list` surfaces the
ones your project still needs.

See `docs/adr/0001-always-vendor-forge-kcl.md` for why vendoring is the only
mechanism.

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
