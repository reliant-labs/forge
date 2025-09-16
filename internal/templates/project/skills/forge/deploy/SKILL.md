---
name: forge/deploy
description: Deploy a Forge project to k3d (local) or a cloud environment via KCL manifests.
when_to_use:
  - You need to push a dev build to a local k3d cluster
  - You're shipping to staging or prod
  - You want to preview the rendered manifests before applying
---

# forge/deploy

Deployments are described in KCL under `deploy/kcl/<env>/` with per-environment overlays (`dev/`, `staging/`, `prod/`). `forge deploy` renders KCL to Kubernetes manifests and applies them.

**Environment is a positional argument, not a flag.** There is no `--env` and no default — bare `forge deploy` fails with a "not enough arguments" error.

## Core commands

```
forge deploy dev                            # apply dev overlay to local k3d
forge deploy staging                        # apply staging overlay
forge deploy prod                           # apply prod overlay (requires explicit "prod")
forge deploy dev --dry-run                  # render and print manifests, do not apply
forge deploy dev --image-tag v1.2.3         # override image tag (default: git short SHA)
forge deploy dev --namespace custom-ns      # override namespace from environment config
```

For dev, `forge deploy dev` ensures a k3d cluster exists and pushes images to the local registry at `localhost:5050` before applying.

## Workflow

1. Render first to eyeball the YAML:
   ```
   forge deploy <env> --dry-run
   ```
   Read the output. KCL is declarative but non-obvious; catching config mistakes here is cheap.
2. Apply:
   ```
   forge deploy <env>
   ```
3. Verify:
   ```
   kubectl get pods -n <namespace>
   kubectl logs -n <namespace> -l app=<service>
   ```

## Rules

- Production deploys require explicitly typing `prod` as the positional argument. There is no implicit default.
- Never `kubectl apply` hand-edited manifests to a shared cluster. Every change goes through `deploy/kcl/<env>/`. Ad-hoc apply creates drift the next `forge deploy` will clobber.
- Secrets never live in KCL. The files under `deploy/kcl/` are checked in — treat them as public. Use cluster-level secret management.
- Image tags must be immutable. Deploy by digest or commit SHA (the default). Do not deploy `:latest` to anything that matters.
- KCL schema changes are forever. Once a field is referenced in a production overlay, removing it is a breaking change — deprecate, don't delete.

## When this skill is not enough

- You need a rollback → `kubectl rollout undo deployment/<name>` for a fast revert, then fix forward via KCL.
- You need to deploy infra (not app workloads) → that's Terraform / Pulumi territory, not forge. `forge deploy` handles only the KCL-defined workloads.
- You need to ship a hotfix without a full CI run → still run a build (`forge build --docker`) and a deploy. Skipping the build is almost never worth it.
