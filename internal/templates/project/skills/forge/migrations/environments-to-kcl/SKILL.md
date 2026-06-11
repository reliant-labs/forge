---
name: environments-to-kcl
description: Migrate forge.yaml `environments[]` into per-env KCL deploy targets. Forge no longer reads `environments:` from forge.yaml at all — env-wide deploy knobs (cluster / namespace / registry / domain) live on per-service `forge.K8sCluster` blocks in KCL, and per-env app config lives in sibling `config.<env>.yaml` files.
applies-from: v1.0.0
applies-to: v2.0.0
detection: grep -l "^environments:" forge.yaml
relevance: migration
---

# Migrating forge.yaml `environments[]` to KCL deploy targets

Use this skill when `forge.yaml` still declares an `environments[]` block.
Forge silently ignores the block now — it never reads it for anything —
but leaving it in place hides intent. Move every field to its new home.

## 1. What changed

Forge v1.x kept env-wide deploy info on `forge.yaml -> environments[]`:

```yaml
# forge.yaml — pre-v2 shape
environments:
  - name: dev
    type: local
    cluster: k3d-myapp
    namespace: myapp-dev
    registry: localhost:5050
    config:
      log_level: debug
      log_format: text
  - name: prod
    type: cloud
    cluster: gke_acme-prod_us-central1_c1
    namespace: myapp-prod
    registry: ghcr.io/acme/myapp
    domain: myapp.com
    config:
      log_level: warn
      database_url: ${prod-db-credentials}
```

Forge v2.x removes the block entirely. Its fields move to two places:

- **Deploy target** (`cluster` / `namespace` / `registry` / `domain`) →
  per-service `forge.K8sCluster` blocks in KCL.
- **Per-env app config** (`config:` map) → sibling `config.<env>.yaml`
  files next to `forge.yaml`.

Forge no longer reads `environments:` for anything — leaving it in
forge.yaml is harmless (silently ignored) but misleading. Strip it.

```kcl
import forge

# Declare once at the top — share via ref across many services.
_prod_k8s = forge.K8sCluster {
    cluster = "gke_acme-prod_us-central1_c1"
    namespace = "myapp-prod"
    registry = "ghcr.io/acme/myapp"
    domain = "myapp.com"
}

_bundle = forge.Bundle {
    services = [
        forge.Service { name = "api",    deploy = _prod_k8s }
        forge.Service { name = "worker", deploy = _prod_k8s | { replicas = 3 } }
        forge.Service { name = "admin",  deploy = _prod_k8s | { replicas = 1 } }
    ]
}
```

KCL refs DRY the common case (one `_prod_k8s` ref attached to many
services) and `|` merge lets per-service overrides shadow specific
fields without re-declaring the whole block.

Why move? Three reasons:

1. **One source of truth.** Pre-v2, you had to keep
   `forge.yaml -> environments[<env>].namespace` and
   `deploy/kcl/<env>/main.k`'s `RenderEnv { namespace = ... }` in
   sync. v2 collapses both onto the KCL `K8sCluster` block — the
   forge CLI reads it from there directly.
2. **Multi-cluster per env.** Pre-v2 implicitly assumed one cluster
   per env. v2 lets a single env target multiple clusters (e.g. an
   "edge" service on a regional cluster + the rest on the main one);
   each `Service.deploy` picks its own `K8sCluster` ref.
3. **Non-cluster deploy targets.** v2 adds `forge.External` (generic
   shell-command escape hatch for Fly.io / Cloud Run / Cloudflare
   Workers / ECS / etc.) and `forge.Compose` (docker-compose) as
   sibling deploy-target schemas. They can't fit on `forge.yaml ->
   environments[]` (the env-wide fields are cluster-specific) and now
   slot in cleanly alongside `K8sCluster`.

## 2. Detection

```bash
# Pre-v2 shape: environments[] in forge.yaml.
grep -E "^environments:" forge.yaml

# Per-env, inspect:
grep -A6 "^environments:" forge.yaml
```

Forge does NOT print a warning when the block is present — it's
silently ignored. The detection above is the only signal.

## 3. Migration (deterministic part)

For each environment in forge.yaml's `environments[]`:

1. Declare a matching `_<env>_k8s = forge.K8sCluster { ... }` variable
   at the top of `deploy/kcl/<env>/main.k` and attach it to each service.
2. Move the `config:` map (if present) into a sibling
   `config.<env>.yaml` file at the project root, next to forge.yaml.
3. Delete the env entry from `forge.yaml`.

```bash
# 1. Inspect every env's forge.yaml block.
grep -A8 "^environments:" forge.yaml
```

Worked example — before:

```yaml
# forge.yaml
environments:
  - name: prod
    type: cloud
    cluster: gke_acme-prod_us-central1_c1
    namespace: myapp-prod
    registry: ghcr.io/acme/myapp
    domain: myapp.com
    config:
      log_level: warn
      database_url: ${prod-db-credentials}
```

```kcl
# deploy/kcl/prod/main.k
import forge

_bundle = forge.Bundle {
    services = [
        forge.Service { name = "api",    deploy = forge.K8sDeploy { replicas = 2, ports = [8080] } }
        forge.Service { name = "worker", deploy = forge.K8sDeploy { replicas = 3 } }
        forge.Service { name = "admin",  deploy = forge.K8sDeploy { replicas = 1 } }
    ]
}

output = forge.render(_bundle)
```

After:

```kcl
# deploy/kcl/prod/main.k
import forge

# Env-wide knobs declared once, shared across every service.
_prod_k8s = forge.K8sCluster {
    cluster = "gke_acme-prod_us-central1_c1"
    namespace = "myapp-prod"
    registry = "ghcr.io/acme/myapp"
    domain = "myapp.com"
}

_bundle = forge.Bundle {
    services = [
        # KCL `|` merge attaches per-service overrides on top of the ref.
        forge.Service { name = "api",    deploy = _prod_k8s | { replicas = 2, ports = [8080] } }
        forge.Service { name = "worker", deploy = _prod_k8s | { replicas = 3 } }
        forge.Service { name = "admin",  deploy = _prod_k8s | { replicas = 1 } }
    ]
}

output = forge.render(_bundle)
```

```yaml
# config.prod.yaml (sibling file next to forge.yaml)
log_level: warn
database_url: ${prod-db-credentials}
```

```yaml
# forge.yaml — environments[] block removed entirely.
# Per-env config now lives in config.<env>.yaml sibling files.
```

Repeat for every env (dev, staging, prod, …).

## 4. Migration (manual part)

What user code / config might need to change:

- **Per-service overrides.** Pre-v2 there was no way to override
  `namespace` per service in an env (every service in an env shared
  one namespace). v2 lets each `forge.Service.deploy` point at a
  different `K8sCluster` ref, so a single env can deploy one service
  to a "shared" cluster and another to a "regional" cluster. Common
  shape:

  ```kcl
  _main_k8s   = forge.K8sCluster { cluster = "...", namespace = "..." }
  _edge_k8s   = forge.K8sCluster { cluster = "...", namespace = "..." }

  services = [
      forge.Service { name = "api",        deploy = _main_k8s }
      forge.Service { name = "edge-proxy", deploy = _edge_k8s }
  ]
  ```

- **Per-env config (`environments[].config`).** Move the map to a
  sibling `config.<env>.yaml` file at the project root. Forge reads
  `config.<env>.yaml` automatically for the per-env ConfigMap
  projection AND for `forge run` / `forge up` host-mode env injection.

- **CI workflows that read `environments[]` to gate deploy jobs.**
  Update them to source the env list from the filesystem
  (`ls deploy/kcl/`) instead. Forge's own generated CI workflows
  already use `forge ci validate-kcl` which discovers envs by
  filesystem walk.

- **Per-env cluster guard.** Pre-v2 the guard read
  `environments[<env>].cluster` to know which context
  `forge deploy <env>` was supposed to land in. v2 reads it from
  `K8sCluster.cluster` instead. Forge no longer reads
  `environments[].cluster` at all.

## 5. Verification

```bash
# Confirm KCL renders cleanly for every env after the rewrite.
for env in deploy/kcl/*/; do
    kcl run "$env" --format json > /dev/null && echo "$env: OK" || echo "$env: FAIL"
done

# Confirm forge deploy --dry-run still produces the same manifests.
forge deploy dev --dry-run > /tmp/v2-dev.yaml
git stash
forge deploy dev --dry-run > /tmp/v1-dev.yaml
git stash pop
diff /tmp/v1-dev.yaml /tmp/v2-dev.yaml  # should be empty

# Confirm `environments:` is no longer in forge.yaml.
grep -E "^environments:" forge.yaml  # should print nothing
```

## 6. Rollback

If the v2 shape breaks something:

```bash
# Restore forge.yaml and deploy/kcl/ from git.
git checkout HEAD -- forge.yaml deploy/kcl/ config.*.yaml

# Downgrade the CLI binary via forge upgrade.
forge upgrade --to <prior-v1-version>
```

The v1 shape works unchanged in v1. The schema change is one-way
(`environments[]` removed in v2) — restoring forge.yaml + the older
binary is the supported rollback.

## Per-env conditional includes

The forge CLI passes the current env name to KCL as `-D env=<env>` on
every `kcl run` invocation. Use `option("env")` in `main.k` to gate
fields per-env — typically `additional_manifests` for in-cluster infra
that should ship to k3d / staging / prod but not to `dev-host` envs
where docker-compose provides the same services:

```kcl
_env_name = option("env")
_is_dev_host = _env_name == "dev-host"

_bundle = forge.Bundle {
    additional_manifests = [] if _is_dev_host else [
        # in-cluster NATS, Temporal, LiteLLM, etc.
    ]
}
```

See `kcl/README.md` for the full pattern.

## See also

- `architecture` skill — where K8sCluster sits in the deploy/kcl
  module relative to HostDeploy / External / Compose.
- `deploy` skill — the per-group dispatch architecture and how
  K8sClusterProvider wraps the existing cluster.Apply pipeline.
- `v0.x-to-env-config` skill — the parallel migration that introduced
  per-env config; the `config.<env>.yaml` sibling-file path is now
  the canonical home for those values.
