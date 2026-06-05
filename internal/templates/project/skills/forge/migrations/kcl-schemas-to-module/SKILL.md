---
name: kcl-schemas-to-module
description: Migrate a forge project's in-tree `deploy/kcl/schema.k` / `base.k` / `render.k` / `lib/*.k` to the upstream `forge` KCL module. ~3000 lines of duplicated schema deletes; projects import typed entities (Service / Operator / Frontend / CronJob with polymorphic deploy) instead of constructing the legacy `Application` struct.
---

# Migrating in-tree KCL schemas to the upstream forge module

Use this skill when `forge upgrade` reports a jump across the version
that hoisted the KCL schemas out of every project (`1.x → 2.0`). It
only affects projects that have `deploy/kcl/schema.k`, `base.k`,
`render.k`, and a `deploy/kcl/lib/` subtree in their working tree —
all of which become redundant after the upstream module ships.

## 1. What changed

Before: every forge project's `forge new` / `forge generate` copied
~3000 lines of KCL schema + render logic into `deploy/kcl/`. Schema
changes couldn't propagate — fixing a bug in `render.k` meant
hand-porting it across every project. And the legacy `Application`
struct was overloaded: Services, Operators, Frontends, CronJobs all
squeezed through one shape so the forge CLI couldn't tell intent from
type.

After: forge ships a published KCL module (`forge/kcl/`) with four
typed entities — `Service` / `Operator` / `Frontend` / `CronJob` —
plus a polymorphic deploy union (`HostDeploy | K8sDeploy | BuildOnly`)
that lets the CLI dispatch on intent. Projects depend on the module
and `import forge`; the schemas live upstream and version under
`kcl-vX.Y.Z` git tags.

The win:

- Schema changes propagate via `forge upgrade` bumping the module
  version pin in `kcl.mod`.
- Intent is typed — `Operator` is no longer "an Application that
  happens to also need cluster-scoped RBAC."
- `Service.deploy` is a typed union, not a string field — the CLI
  knows `HostDeploy` means host-run, `K8sDeploy` means cluster-run,
  `BuildOnly` means build only.
- `~3000 lines per project` of schema/render code goes away.

## 2. Detection

```bash
# Old shape: schema.k / base.k / render.k live in your project's tree.
ls deploy/kcl/schema.k deploy/kcl/base.k deploy/kcl/render.k 2>/dev/null

# New shape: kcl.mod has the upstream KCL module pinned.
grep -E "^[[:space:]]*forge[[:space:]]*=" deploy/kcl/kcl.mod 2>/dev/null
```

If both the legacy files exist AND `forge` isn't a kcl.mod dependency
yet, this migration applies.

## 3. Migration (deterministic part)

### Step 1: pin the forge module in `deploy/kcl/kcl.mod`

```toml
[package]
name = "myapp"
edition = "v0.11.0"
version = "0.0.1"

[dependencies]
forge = { git = "https://github.com/reliant-labs/forge.git", tag = "kcl-v0.1.0" }
```

For local development against a checked-out forge clone:

```toml
[dependencies]
forge = { path = "/path/to/forge/kcl" }
```

### Step 2: delete the in-tree schemas

```bash
rm deploy/kcl/schema.k deploy/kcl/base.k deploy/kcl/render.k
rm -rf deploy/kcl/lib  # if a lib/ subdir exists from forge add crd / similar
```

Project-specific `deploy/kcl/lib/<x>_crd.k` files that `forge add crd`
emitted stay — those are owned by the project. The module ships the
`forge.crd(...)` HELPER but doesn't ship project CRDs.

### Step 3: rewrite each `deploy/kcl/<env>/main.k`

The shape goes from the legacy `Application` + flat string-typed
deploy field:

```kcl
# BEFORE
import deploy.kcl.schema
import deploy.kcl.render

_env = schema.Environment {
    name = "dev"
    namespace = "myapp-dev"
    applications = {
        "admin-server" = schema.Application {
            name = "admin-server"
            image = "myapp"
            replicas = 1
            ports = [schema.ServicePort {port = 80, target_port = 8080}]
            env_vars = [...]
        }
    }
}

manifests = render.render_environment(_env)
```

…to the typed Bundle + polymorphic deploy:

```kcl
# AFTER
import forge

_bundle = forge.Bundle {
    services = [
        forge.Service {
            name = "admin-server"
            image = "myapp"
            deploy = forge.K8sDeploy {
                replicas = 1
                ports = [8080]
            }
        }
    ]
    operators = []
    frontends = []
    cronjobs = []
}

# JSON contract that forge build/run/deploy consumes.
output = forge.render(_bundle)

# K8s manifest list for `kcl run | kubectl apply -f -`.
_env = forge.RenderEnv {
    namespace = "myapp-dev"
    image_registry = option("registry") or "localhost:5050"
    image_tag = option("image_tag") or "latest"
    network_policies = False
}
manifests = forge.render_manifests(_bundle, _env)
```

Per-entity mapping rules:

| Legacy shape | New shape |
|---|---|
| `Application` + the service is host-run | `Service` with `deploy = forge.HostDeploy{...}` |
| `Application` + Deployment + Service | `Service` with `deploy = forge.K8sDeploy{...}` |
| `Application` + `cluster_rbac = True` | `Operator` with `cluster_rbac = forge.ClusterRBAC{rules = [...]}` |
| `Application` for a frontend container | `Frontend` (note: dev runs on host now, prod still ships a container; the CLI handles it) |
| `Application` for a binary CLI | `Service` with `deploy = forge.BuildOnly{...}` |
| Hand-built CronJob manifest | `forge.CronJob{schedule = "@hourly", ...}` |

### Step 4: regenerate per-env config + verify

```bash
forge generate
forge build --env dev   # exercises the JSON contract
forge deploy dev --dry-run   # exercises render_manifests
```

`forge generate` emits the per-env `config_gen.k` files (project-
specific config, NOT touched by this migration). The new `main.k`
imports them the same way it always did:

```kcl
import deploy.kcl.dev.config_gen as cfg
# ...
env_vars = cfg.APP_ENV
```

## 4. Migration (manual part)

What user code might need to change:

- **`Environment.additional_manifests`**. The new shape doesn't
  expose this field on `Bundle`. If your project appends ClusterIssuers
  / SealedSecrets / hand-typed CRDs there, copy them into a sibling
  `additional.k` and concat in `main.k`:
  ```kcl
  import additional
  manifests = forge.render_manifests(_bundle, _env) + additional.MANIFESTS
  ```
- **HPA + PDB.** The new typed shape doesn't ship these out of the
  box (most projects scale via Deployment.replicas + manual PDB).
  Either author them in `additional.k` or open an issue requesting a
  `forge.AutoScaling` / `forge.PodDisruptionBudget` typed sub-schema.
- **MultiServiceApplication / SubCommandService.** These shapes are
  gone — the typed `Service` already supports per-service `command:`
  override and the forge CLI handles the shared-binary build matrix.
  If you used `binary: shared` in `forge.yaml`, the regenerated
  scaffold takes care of it; you don't reconstruct the multi-service
  application in KCL anymore.
- **Hand-written `lib/services.k` / `lib/rbac.k` etc. helpers.** Delete
  them — the upstream module ships equivalents.
- **`base.k` constants.** The upstream `forge.DB_ENV` /
  `forge.OTEL_ENV` cover the common cases. Project-specific bundles
  (e.g. `STRIPE_ENV`) stay in `base.k` next to your `main.k`. Adjust
  the import: instead of `import deploy.kcl.base`, you can either
  inline the list in `main.k` or keep a local `base.k` file and
  `import base` from `main.k`.

## 5. Verification

```bash
# 1. KCL still parses and the JSON contract holds.
cd deploy/kcl
kcl run dev -S output --format json | jq .

# Expect: { "services": [...], "operators": [...], "frontends": [...], "cronjobs": [...], "config_maps": [...] }
# Each `services[].deploy` should have a `type` discriminator
# ("host" | "cluster" | "build-only") or be null.

# 2. Manifests render to YAML kubectl accepts.
kcl run dev/main.k -S manifests | kubectl apply --dry-run=client -f -

# 3. The forge build + deploy round-trip still works.
forge generate
forge build --env dev
forge deploy dev --dry-run
```

## 6. Rollback

```bash
git checkout HEAD -- deploy/kcl/
forge upgrade --to <prior-version>
```

The prior version's `forge generate` re-emits the in-tree schemas.
Note: `--to <prior-version>` requires the older forge build on PATH.

## See also

- `architecture` skill — where the KCL module sits in the deploy pipeline.
- `deploy` skill — how `forge deploy <env>` consumes both `output`
  (JSON contract) and `manifests` (k8s YAML).
- `forge/kcl/README.md` — the module's user-facing reference for the
  four typed entities and the polymorphic deploy union.
