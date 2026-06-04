---
name: environments-to-kcl
description: Migrate forge.yaml `environments[]` into per-env KCL deploy targets. Forge v1.x kept env-wide deploy knobs (cluster / namespace / registry / domain) on `forge.yaml -> environments[]`; v2.x moves them onto per-service `forge.K8sCluster` blocks in KCL, with refs DRYing the common case across many services.
applies-from: v1.0.0
applies-to: v2.0.0
detection: grep -l "^environments:" forge.yaml
---

# Migrating forge.yaml `environments[]` to KCL deploy targets

Use this skill when `forge upgrade` reports a jump that crosses v2.0.0
and `forge.yaml` still declares one or more `environments[]` entries
with `cluster:` / `namespace:` / `registry:` / `domain:` fields.

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
  - name: prod
    type: cloud
    cluster: gke_acme-prod_us-central1_c1
    namespace: myapp-prod
    registry: ghcr.io/acme/myapp
    domain: myapp.com
```

KCL's `K8sDeploy` carried only per-service knobs (replicas / ingress /
ports / platform); the env-wide info had to live in forge.yaml because
each Service couldn't carry the env identifier itself.

Forge v2.x introduces a NEW per-service deploy target schema
(`forge.K8sCluster`) that carries BOTH the env-wide knobs AND the
per-service knobs:

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
3. **Future deploy targets.** v2 adds `forge.VMDocker` and
   `forge.Compose` as sibling deploy-target schemas. They can't fit on
   `forge.yaml -> environments[]` (the env-wide fields are cluster-
   specific) and now slot in cleanly alongside `K8sCluster`.

The legacy `forge.yaml -> environments[]` block keeps working for one
migration cycle so existing projects don't break. Forge emits a
deprecation notice on every CLI invocation until the block is removed.

## 2. Detection

```bash
# Pre-v2 shape: environments[] in forge.yaml with cluster/namespace/registry fields.
grep -E "^environments:" forge.yaml

# Per-env, inspect:
grep -A6 "^environments:" forge.yaml
```

If `forge` emits

> [forge] notice: `environments[]` in forge.yaml is deprecated.

on every command, that's the same signal.

## 3. Migration (deterministic part)

For each environment in forge.yaml's `environments[]`, declare a
matching `_<env>_k8s = forge.K8sCluster { ... }` variable at the top
of `deploy/kcl/<env>/main.k` and attach it to each service.

```bash
# 1. Inspect every env's forge.yaml block.
grep -A6 "^environments:" forge.yaml

# 2. For each env, edit deploy/kcl/<env>/main.k. Example for prod:
#    BEFORE — forge.yaml carries the env-wide fields; KCL only has
#    K8sDeploy with per-service replicas/ingress/ports.
#    AFTER — KCL's _prod_k8s ref carries everything; forge.yaml's
#    environments[prod] block becomes a stub (or is removed entirely).
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
manifests = forge.render_manifests(_bundle, forge.RenderEnv {
    namespace = "myapp-prod"
    image_registry = "ghcr.io/acme/myapp"
})
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
manifests = forge.render_manifests(_bundle, forge.RenderEnv {
    namespace = _prod_k8s.namespace
    image_registry = _prod_k8s.registry
})
```

```yaml
# forge.yaml — environments[] block can now be removed (or kept as a
# stub for the `name`/`type` fields if other tooling still consults it).
# The deploy-target fields (cluster/namespace/registry/domain) ALL
# move to KCL.
environments:
  - name: prod
    type: cloud
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

- **Per-env config (forge.yaml `environments[].config`).** That field
  is OUT OF SCOPE for this migration — it survives unchanged. v2
  keeps `environments[].config` as the canonical source for the
  per-env ConfigMap projection (`(forge.v1.config)`-annotated proto
  fields). Only the deploy-target fields (cluster/namespace/registry/
  domain) move.

- **CI workflows that read `environments[]` to gate deploy jobs.**
  Update them to source the env list from the filesystem
  (`ls deploy/kcl/`) instead. Forge's own generated CI workflows
  already use `forge ci validate-kcl` which discovers envs by
  filesystem walk.

- **forge.yaml `environments[].cluster` for the kubectl-context guard.**
  Pre-v2 the guard read `environments[<env>].cluster` to know which
  context `forge deploy <env>` was supposed to land in. v2 reads it
  from `K8sCluster.cluster` instead. Until you remove `environments[]`
  entirely, the legacy field still works; once it's gone, the guard
  picks up the K8sCluster ref automatically.

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

# Confirm the deprecation notice stops firing once `environments[]`
# is removed from forge.yaml.
forge audit 2>&1 | grep -i "deprecat"  # should be empty
```

## 6. Rollback

If the v2 shape breaks something:

```bash
# Restore forge.yaml and deploy/kcl/ from git.
git checkout HEAD -- forge.yaml deploy/kcl/

# Downgrade the CLI binary via forge upgrade.
forge upgrade --to <prior-v1-version>
```

The v1 shape works unchanged in v1 — the schema change is
additive-then-deprecated (K8sCluster added, K8sDeploy deprecated but
still functional), so reverting the KCL + forge.yaml restores the v1
loop without binary downgrade in most cases.

## See also

- `architecture` skill — where K8sCluster sits in the deploy/kcl
  module relative to HostDeploy / VMDocker / Compose.
- `deploy` skill — the per-group dispatch architecture and how
  K8sClusterProvider wraps the existing cluster.Apply pipeline.
- `v0.x-to-env-config` skill — the parallel migration that introduced
  `forge.yaml -> environments[].config`; the per-env config map
  survives this migration unchanged.
