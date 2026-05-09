---
name: deploy
description: Ship code — lint, build, deploy to k3d/staging/prod, and verify.
---

# Ship It

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
forge build             # binary + frontends
forge build --docker    # Docker images for all services
forge build --debug     # with debug symbols for Delve
```

## Deploy

Environment is a positional arg — `forge deploy dev`, not `forge deploy --env dev`.

```
forge deploy dev              # local k3d (auto-creates cluster, pushes to localhost:5050)
forge deploy staging          # staging environment
forge deploy prod             # production (must explicitly type "prod")
forge deploy dev --dry-run    # render manifests without applying — eyeball the YAML first
forge deploy dev --image-tag  # override image tag (default: commit SHA)
```

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
`proto/config/v1/config.proto` + `forge.yaml -> environments[<env>].config`.
A few things are surprising on first read:

- **Default-only fields don't appear.** If a proto field has a
  `default_value:` and no per-env override in `forge.yaml`, the
  generated KCL skips the env-var entirely. The binary applies the
  default at startup via `pkg/config/config.go::Load()`. The rendered
  Deployment will NOT show the default as a literal env var, and that
  is intentional — defaults are the binary's contract, not the
  manifest's. To make a default visible in the rendered manifest, add
  the value (or a `${secret-ref}`) to `environments[<env>].config`.
- **Empty categories are elided.** A category that ends up with no
  emitted entries (e.g. all of its fields are non-sensitive defaults)
  is skipped from the generated file rather than emitting an empty
  `<CATEGORY>_ENV: [schema.EnvVar] = []`. If your `main.k` references
  `cfg.<CATEGORY>_ENV` and KCL errors with "no attribute", regenerate
  and concatenate only the categories that actually produced entries.
- **Sensitive fields always emit.** Every `(forge.v1.config) = {
  sensitive: true }` field projects to a `secret_ref` EnvVar
  regardless of whether the env config provides a `${...}` override —
  the project-level default secret name + lowercased env-var key
  apply unconditionally.

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
