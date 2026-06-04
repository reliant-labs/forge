---
name: dev-target-to-kcl-deploy
description: Migrate from forge.yaml `services[].dev_target` (host|cluster) to the polymorphic KCL `Service.deploy` field (forge.HostDeploy | forge.K8sDeploy | forge.BuildOnly). The deploy target is an environment + KCL concern, not a service-shape concern; same service can deploy differently per env.
applies-from: v0.5.0
applies-to: v0.6.0
detection: grep -l "dev_target" forge.yaml
---

# Migrating from forge.yaml `dev_target` to KCL `Service.deploy`

## What changed

forge.yaml's `services[].dev_target: host | cluster` field (shipped briefly
in commit `cd25640`) was REMOVED. The deployment target now lives on the
KCL `Service.deploy` field as a polymorphic discriminated union:
`forge.HostDeploy | forge.K8sDeploy | forge.BuildOnly`.

Why: deployment target is an environment + KCL concern, not a service-
shape concern. Same service can deploy differently per env (typically host
in dev, cluster in staging/prod).

## Detection

A pre-upgrade project has:

```bash
grep -r "dev_target" forge.yaml
```

If that returns any line, this migration applies.

## Migration steps

For each service in forge.yaml with `dev_target: host`:

1. **Remove the field from forge.yaml.** Plain delete; nothing replaces
   it at the forge.yaml level.

2. **Open `deploy/kcl/<env>/main.k`** for each env that should have a
   host deploy (typically `dev` only — staging/prod stay cluster).

3. **Find the `forge.Service { name = "<that-service>" ... }` call.**
   It currently has `deploy = forge.K8sDeploy {...}` or no deploy field.

4. **Replace the deploy assignment with `forge.HostDeploy`:**

   ```kcl
   forge.Service {
       name = "admin-server"
       image = "cp-forge:dev"
       deploy = forge.HostDeploy {
           runner = "air"           # or "go-run" | "binary" | "delve"
           air_config = ".air.toml" # required when runner == "air"
           env_file = ".env.dev"    # defaults to .env.<env>
       }
   }
   ```

5. **Run `forge generate`** to verify the KCL still parses.
6. **Run `forge up --env=dev`** to verify the new shape works end-to-end.

## Rollback

If you need to back out: re-add `dev_target: host` to forge.yaml AND
downgrade forge to a version that supports it (pre commit `<TBD>` —
fill in once the revert commit lands). Both halves required.

## Affected projects

Run `grep -r dev_target forge.yaml` in each project worktree to identify.
At time of writing: cp-forge, kalshi-trader (last known to have it).

## Related migrations

- `kcl-schemas-to-module` — likely run BEFORE this one, since the new
  KCL `forge.HostDeploy` schema lives in the forge KCL module.
