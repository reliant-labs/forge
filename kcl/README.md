# forge — KCL module

Typed schemas + manifest render layer that forge projects import.

```kcl
import forge

forge.Service {
    name = "admin-server"
    image = "myapp:dev"
    deploy = forge.HostDeploy { runner = "air", air_config = ".air.toml" }
}

forge.Service {
    name = "workspace-proxy"
    image = "myapp:dev"
    deploy = forge.K8sDeploy {
        replicas = 1
        ingress = forge.Ingress { host = "workspaces.localhost", tls = False }
    }
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
`K8sDeploy`, or `BuildOnly`. The `type` discriminator lives ON the
deploy subschema so KCL's JSON output is self-describing. Forge's CLI
reads the discriminator to decide whether to run on host, schedule in
cluster, or just produce a build artifact.

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
