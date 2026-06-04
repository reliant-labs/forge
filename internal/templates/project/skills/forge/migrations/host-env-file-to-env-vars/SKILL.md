---
name: host-env-file-to-env-vars
description: Split HostDeploy's env_file into KCL-declared env_vars (reproducible per-env config) + an external secrets_file (gitignored, dotenv with API keys only). v0.6 conflated config and secrets in one env_file path; v0.7 separates them so host services see the same per-env config source K8sDeploy services see via the Deployment env block.
applies-from: v0.6.0
applies-to: v0.7.0
detection: grep -l "env_file" deploy/kcl/
---

# Migrating HostDeploy env_file → env_vars + secrets_file

Use this skill when `forge upgrade` reports a jump that crosses v0.7.0
and the project's `deploy/kcl/<env>/main.k` declares one or more
`forge.HostDeploy { env_file = "..." }` blocks.

## 1. What changed

Forge v0.6 and earlier modeled host-mode env composition with a single
`env_file` path on `HostDeploy`. That conflated two responsibilities:

  * **Per-env config** — DATABASE_URL, NATS_URL, PORT, LOG_LEVEL, …
    Reproducible across machines; belongs in version control.
  * **Secrets** — STRIPE_*, SUPABASE_*, JWT_PUBLIC_KEY, auth tokens.
    Per-developer / per-environment; never committed.

When both lived in the same file, two failure modes followed:

  1. The file had to be gitignored (because of the secrets), so the
     config drifted between developer machines silently.
  2. Host services drifted from cluster services: K8sDeploy projected
     per-env config from `forge.yaml -> environments[].config` into
     the Deployment's `env` block automatically, but HostDeploy only
     saw whatever the developer happened to have in their local
     `.env.<env>` file.

Forge v0.7+ splits the responsibility on the KCL schema:

```kcl
schema HostDeploy:
    type: str = "host"
    runner: str = "go-run"
    air_config?: str
    env_vars: [EnvVar] = []   # KCL-declared per-env config (reproducible)
    secrets_file?: str        # gitignored dotenv (secrets only)
    delve_port: int = 2345
```

`forge run` / `forge up` host phase loads `secrets_file` first (if
set), then layers `env_vars` on top — KCL wins on conflict so
reproducible config can't drift across machines. Host services compose
the same `cfg.APP_ENV` / `base.DB_ENV` slices K8sDeploy services use,
keeping the two surfaces aligned.

## 2. Detection

```bash
# Old shape: env_file on HostDeploy blocks.
grep -rn "env_file" deploy/kcl/ 2>/dev/null

# New shape: env_vars + secrets_file on HostDeploy blocks.
grep -rln "secrets_file\|env_vars" deploy/kcl/ 2>/dev/null
```

If `forge audit` reports `kcl_schema_alignment` divergence on
HostDeploy, that's the same signal.

## 3. Migration (deterministic part)

```bash
# 1. Read your current env_file and classify each KEY=VALUE:
#    - Config: connection strings, port numbers, log levels, feature
#      flags, public identifiers. → goes into KCL env_vars
#    - Secrets: API keys, webhook secrets, JWT keys, auth tokens,
#      service-role keys. → stays in the dotenv (now .env.<env>.secrets)
cat .env.dev | head -40

# 2. Create the gitignored secrets file. Keep only secrets.
mv .env.dev .env.dev.secrets
# Edit .env.dev.secrets — delete the config lines (they move to KCL).

# 3. Create a committed template so future developers know the shape.
cp .env.dev.secrets .env.dev.secrets.example
# Edit .env.dev.secrets.example — blank out the values, document each.

# 4. Update .gitignore — the existing `.env.*` rule already covers
#    .env.dev.secrets; ensure .env.dev.secrets.example is NOT ignored
#    (the `!.env.*.example` exception handles it).
grep -E "\.env\.\*\.example" .gitignore

# 5. In deploy/kcl/<env>/main.k, declare the per-env config slice once
#    and reference it from each HostDeploy. Compose with cfg.APP_ENV /
#    base.DB_ENV the same way cluster services do.
```

Worked example — before:

```kcl
forge.Service {
    name = "admin-server"
    deploy = forge.HostDeploy {
        runner = "air"
        env_file = ".env.dev"
    }
}
```

After:

```kcl
# Once at the top of main.k — shared by every host service.
_host_env = cfg.APP_ENV + [
    forge.EnvVar { name = "DATABASE_URL", value = "postgres://localhost:5434/myapp_dev?sslmode=disable" }
    forge.EnvVar { name = "NATS_URL", value = "nats://localhost:4222" }
    forge.EnvVar { name = "PORT", value = "8090" }
    forge.EnvVar { name = "LOG_LEVEL", value = "debug" }
    # ... other reproducible per-env config
]

forge.Service {
    name = "admin-server"
    deploy = forge.HostDeploy {
        runner = "air"
        env_vars = _host_env
        secrets_file = ".env.dev.secrets"
    }
}
```

## 4. Migration (manual part)

What user code / config might need to change:

- **docker-compose `env_file:` directives.** If docker-compose still
  consumes `.env.dev` for non-host services (postgres, nats, litellm),
  keep a minimal `.env.dev` around as a compat shim — document at the
  top that the canonical config lives in KCL. The cp-forge migration
  did this; see `cp-forge/.env.dev.example` for the shim shape.
- **`config_map_ref` projection.** EnvVars sourced via
  `config_map_ref` / `secret_ref` (the K8sDeploy-shaped channels) are
  fine in `env_vars` — the host runner falls back to the entry's
  inline `value` when those channels apply only to cluster mode. If a
  field needs a different value on host vs cluster, declare two slices
  and reference them from each deploy.
- **CI / local scripts that source `.env.<env>` directly.** Anything
  that did `source .env.dev && go run ...` to pick up DATABASE_URL
  must either source `.env.<env>.secrets` AND wire the KCL env_vars
  manually, or go through `forge run` / `forge up` which does the
  composition for you.
- **The `--env-file` flag on `forge run`.** The flag still exists for
  muscle-memory continuity; its contract is now "override the KCL
  HostDeploy.secrets_file path" — KCL env_vars are layered on top of
  whatever file the flag points at.

## 5. Verification

```bash
# Render KCL and confirm every host service has env_vars + secrets_file.
kcl run deploy/kcl/<env> --format json -S output \
  | jq '[.services[] | {name, host: .deploy | select(.type == "host")}]
        | map(select(.host != null))
        | map({name, env_vars_count: (.host.env_vars | length), secrets_file: .host.secrets_file})'

# Bring the loop up. Host services should see the KCL-declared values
# (set a canary env var in KCL and grep the logs).
forge up --env=<env>
```

## 6. Rollback

If the new shape breaks something:

```bash
# Restore the env_file shape from git.
git checkout HEAD -- deploy/kcl/

# Recreate the combined .env.<env> (config + secrets together).
cat .env.dev.secrets > .env.dev
# Append the KCL-derived config values manually.

# Downgrade.
forge upgrade --to <prior-version>
```

The old shape works unchanged in v0.6 — the schema change is
additive-with-removal (env_file removed; env_vars/secrets_file added),
so reverting both the KCL and the dotenv split restores the old loop.

## See also

- `architecture` skill — where HostDeploy sits in the deploy/kcl
  module.
- `auth` skill — how secrets ultimately project into Kubernetes
  Secrets for cluster mode (so the secrets_file shape stays aligned).
- `v0.x-to-env-config` skill — the parallel migration that introduced
  `forge.yaml -> environments[].config` and `cfg.APP_ENV`; the host
  env composition layers on top of that channel.
