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
