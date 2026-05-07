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
                               # (skips docker build/push + k3d bootstrap; pure render)
forge deploy dev --image-tag  # override image tag (default: commit SHA)
```

## Verify

After every deploy, confirm pods are healthy:

```
kubectl get pods -n <namespace>
kubectl logs -n <namespace> -l app=<service>
```

## Per-environment config

Per-env runtime config lives in two places, and `forge generate` projects
both into `deploy/kcl/<env>/config_gen.k`:

1. **Inline in forge.yaml** under `environments[<name>].config`. Good for
   non-secret toggles (log levels, feature flags, sampler rates).
2. **Sibling file `config.<env>.yaml`** next to forge.yaml. Same flat
   key→value shape; values here override the inline map. Good for prod
   where most values are secret refs.

Field metadata is on the proto:

```proto
message AppConfig {
  string database_url = 3 [(forge.v1.config) = {
    env_var: "DATABASE_URL",
    sensitive: true,                 // emit secret_ref, never inline
    description: "PostgreSQL connection string"
  }];

  string stripe_api_key = 100 [(forge.v1.config) = {
    env_var: "STRIPE_API_KEY",
    sensitive: true,
    category: "stripe",              // groups into STRIPE_ENV list
    description: "Stripe API key"
  }];
}
```

Rules `forge generate` follows when emitting `config_gen.k`:

- **Sensitive fields** become `EnvVar { secret_ref = "<project>-secrets",
  secret_key = "<field>" }`. A `${secret-name}` value override in the
  per-env config swaps the `secret_ref` to that name.
- **Non-sensitive fields with a value** become `EnvVar { value = "..." }`.
- **Non-sensitive fields without a value** are skipped — the binary's
  proto-default applies at startup.
- **Categorised fields** land in their own `<CATEGORY>_ENV` list;
  uncategorised fields go in `APP_ENV`.

The hand-edited `main.k` for the env imports `deploy.kcl.<env>.config_gen`
and concatenates `cfg.APP_ENV` + `cfg.STRIPE_ENV` + ... into
`Application.env_vars`. Don't hand-edit `config_gen.k` — `forge generate`
overwrites it.

`forge run --env dev` reads the same per-env config and exports it as
process env-vars to the running binary (sensitive fields are skipped —
set those locally via direnv / .env).

`forge deploy <env>` passes non-sensitive scalars to KCL via `-D
<key>=<value>` so `main.k` can also bind them as top-level identifiers.

## MultiServiceApplication — one image, many Deployments

When a project ships a single Go binary that exposes N cobra subcommands
— the canonical case is `forge.yaml` `binary: shared` (Layer A schema +
Layer B codegen, both shipped) — use `MultiServiceApplication` instead
of N copies of `Application`. The image build/push runs once; each
Deployment selects its behavior via `args:`.

Forge emits this shape automatically for `binary: shared` projects.
The example below is what `deploy/kcl/<env>/main.k` looks like in a
shared-binary project (and what you'd hand-write in a project that
predates the shared mode but wants the deploy-time savings).

See the `architecture` skill's "Binary modes" section for when to
pick `binary: shared` vs the default `per-service`, and the
`migration/v0.x-to-binary-shared` skill for migrating an existing
multi-service project.

```kcl
import deploy.kcl.schema
import deploy.kcl.render

multi = schema.MultiServiceApplication {
    name = "platform"
    image = "platform"
    command = ["/usr/local/bin/platform"]   # shared binary path
    shared_env_vars = base.OTEL_ENV          # layered onto every service
    services = [
        schema.SubCommandService {
            name = "api"
            args = ["server", "api"]         # cobra subcommand
            ports = [schema.ServicePort {port = 80, target_port = 8080}]
        }
        schema.SubCommandService {
            name = "worker"
            args = ["worker", "billing"]
            replicas = 2
        }
        schema.SubCommandService {
            name = "operator"
            args = ["operator", "core"]
            service_account_name = "core-operator"
        }
    ]
}

env = schema.Environment {
    name = "dev"
    namespace = "platform-dev"
    image_registry = registry
    image_tag = image_tag
    network_policies = False
    # multi_service_apps expands the MultiServiceApplication into a
    # `{name: Application}` map suitable for Environment.applications.
    applications = render.multi_service_apps(multi)
}

manifests = render.render_environment(env)
```

`render.multi_service_apps(multi)` is the bridge — it returns one
`schema.Application` per `services[*]`, all sharing `multi.image`. Each
inherits the parent's `command` / `shared_env_vars` / `labels` /
`annotations` and merges its own per-service overrides. From there the
existing `render_environment` lambda emits Deployment + Service +
ServiceAccount/Role/RoleBinding per child Application as usual.

When to reach for it:

- Multiple services share one Go binary (cobra subcommands).
- You want to build/push one image instead of N.
- Per-service replicas/resources/env still need to differ.

When to skip it:

- Services are different binaries — keep them as separate
  `Application` entries.
- You need only one service — `Application` directly is simpler.

## Rollback

Fast revert with `kubectl rollout undo deployment/<name>`, then fix forward via KCL. Never leave a rollback as the permanent state.

## Rules

- Never skip lint — `.golangci.yml` and `buf.yaml` are the contract.
- Never `//nolint` without a reason comment: `//nolint:errcheck // best-effort cleanup`.
- Image tags must be immutable — commit SHA by default, never `:latest`.
- Secrets never live in KCL deploy files — they're checked in, treat as public.
- KCL schema changes are forever in production overlays — deprecate, don't delete.
- Don't `kubectl apply` hand-edited manifests — everything through `deploy/kcl/`.
