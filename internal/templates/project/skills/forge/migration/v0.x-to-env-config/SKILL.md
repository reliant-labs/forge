---
name: v0.x-to-env-config
description: Migrate from hand-curated env-var groups in KCL to forge.yaml environments[].config (per-env config) + sensitive-field projection. forge versions before 1.6 emitted env-var soup; 1.6+ projects per-env config to ConfigMap/Secret/value automatically.
---

# Migrating env-var soup to per-environment config

Use this skill when `forge upgrade` reports a jump across the version
that introduced `environments[].config` to `forge.yaml` and per-env
`config_gen.k` to `deploy/kcl/<env>/` (typically `1.5.x → 1.6.x`).
It only affects projects whose `deploy/kcl/base.k` (or sibling
hand-edited KCL) carries hand-curated `<NAME>_ENV` lambdas /
constants.

## 1. What changed

Forge versions before 1.6 expected the user to hand-curate env-var
groups in KCL — a `DB_ENV` constant, a `NATS_ENV` constant, a
`STRIPE_ENV` constant — and concatenate them into each
`Application.env_vars` list per environment. Every new field meant a
hand-edit of `base.k`, every secret meant manually constructing a
`secret_ref`-shaped EnvVar, and dev/staging/prod values drifted
because there was no single source of truth.

Forge 1.6+ inverts the model:

1. **Proto annotations** on `proto/config/v1/config.proto` carry the
   per-field metadata:
   - `(forge.v1.config) = { sensitive: true }` — projects the field to
     a Kubernetes Secret (`secret_ref`-shaped EnvVar) instead of an
     inline value.
   - `(forge.v1.config) = { category: "stripe" }` — groups related
     fields under a named `<CATEGORY>_ENV` slice (e.g. all
     `category: "stripe"` fields land in the generated `STRIPE_ENV`).
2. **`forge.yaml`** gained `environments[<name>].config` — a per-env
   key-value map keyed by proto field name (snake_case). Use it for
   dev/staging values that aren't secret. Sensitive values can be
   `${secret-ref-name}` strings to override the default secret name.
3. **Sibling files** `config.<env>.yaml` next to `forge.yaml` are
   merged on top of the inline map (sibling-wins). Use them for prod
   where you don't want non-secret toggles cluttering `forge.yaml`.
4. **`forge generate`** emits `deploy/kcl/<env>/config_gen.k` for every
   env. Each `main.k` imports it as `cfg` and concatenates
   `cfg.APP_ENV + cfg.<CATEGORY>_ENV` into `Application.env_vars`.
5. **`forge run --env <env>`** projects the merged per-env config to
   subprocess env vars (sensitive fields skipped — set those locally
   via direnv / .env).
6. **`forge deploy <env>`** passes non-sensitive scalars to KCL via
   `-D key=value`.

The win: one declarative source per environment, sensitive-field
projection is automatic, and dev/staging/prod drift is caught by
schema (proto field names are typed).

## 2. Detection

```bash
# Old shape: hand-curated <NAME>_ENV lambdas / lists in KCL.
grep -E "^[A-Z_]+_ENV[[:space:]]*=" deploy/kcl/base.k 2>/dev/null
# Look for: DB_ENV = ..., NATS_ENV = ..., STRIPE_ENV = ..., AUTH_ENV = ...

# New shape: per-env generated config module exists.
ls deploy/kcl/*/config_gen.k 2>/dev/null
```

Plus: if `forge audit` reports `proto_migration_alignment` divergence
in the config proto, that's a related signal that the project's
`proto/config/v1/config.proto` is missing the new annotations.

## 3. Migration (deterministic part)

```bash
# 1. Audit existing env-var groups in deploy/kcl/. Note which fields
#    carry secret values vs plain config.
grep -A20 "_ENV[[:space:]]*=" deploy/kcl/base.k

# 2. Annotate sensitive fields in proto/config/v1/config.proto:
#       string database_url = 3 [(forge.v1.config) = { sensitive: true }];
#       string stripe_api_key = 4 [(forge.v1.config) = { sensitive: true, category: "stripe" }];
#       string log_level = 5 [(forge.v1.config) = { category: "app" }];

# 3. Move each field's per-env value into forge.yaml's
#    environments.<env>.config: map (inline). For prod-flavored,
#    secret-heavy envs, prefer a sibling config.<env>.yaml file.

# 4. Regenerate KCL.
forge generate

# 5. Inspect the result. Each env now has a config_gen.k that
#    auto-projects sensitive→secretKeyRef, regular→value/ConfigMap.
cat deploy/kcl/dev/config_gen.k
cat deploy/kcl/prod/config_gen.k

# 6. In each main.k, replace
#       env_vars = base.DB_ENV + base.NATS_ENV + base.STRIPE_ENV
#    with
#       env_vars = cfg.APP_ENV + cfg.STRIPE_ENV

# 7. Once every field has moved, delete the hand-curated <NAME>_ENV
#    lambdas from base.k.
```

Steps 2, 4, and 5 run automatically inside `forge upgrade`. Steps 1,
3, 6, and 7 are LLM-or-human work — they touch hand-written code that
forge can't safely rewrite with a regex.

## 4. Migration (manual part)

What user code / config might need to change:

- **Per-env value differences.** Dev / staging / prod often have
  different log levels, feature flags, regional endpoints. The new
  shape makes per-env override first-class — review each field and
  decide whether to inline a single value (`config:` at the top
  level) or split it across envs.
- **Secret name conventions.** The default secret name forge picks
  for a `sensitive: true` field is `<project>-secrets` and the default
  key is the env-var lowercased (e.g. env_var `DATABASE_URL` → key
  `database_url`). If your cluster already has secrets under different
  names, override per-env with a `${actual-secret-name}` string in
  `config.<env>.yaml` / `environments.<env>.config`. To override the
  key as well — useful for legacy clusters whose secrets store data
  under kebab-case keys (`database-url`, `service-role-key`) rather
  than the forge default — use `${secret-name#secret-key}`:
  ```yaml
  config:
    database_url: "${db-credentials#database-url}"
    supabase_jwt_secret: "${reliant-admin#supabase-jwt-secret}"
  ```
- **Non-config env vars (e.g. `OTEL_*`, `KUBERNETES_*`).** Don't try
  to fold these into `proto/config/v1/config.proto` — they're
  infrastructure, not app config. Keep them in
  `base.OTEL_ENV` / similar, then concatenate alongside `cfg.APP_ENV`
  in each `main.k`.
- **`forge run` consumers.** If the project relies on `forge run`
  exporting specific env-var names, double-check the snake-to-
  SCREAMING_SNAKE conversion (proto `database_url` →
  env `DATABASE_URL`).

### Worked example: one field

Before — hand-curated in `deploy/kcl/base.k`:

```kcl
DB_ENV = lambda env: schema.Environment -> [schema.EnvVar] {
    [
        schema.EnvVar {
            name = "DATABASE_URL"
            secret_ref = schema.SecretRef {
                name = env.namespace + "-db-credentials"
                key = "url"
            }
        }
    ]
}
```

After — `proto/config/v1/config.proto`:

```proto
message Config {
  string database_url = 3 [(forge.v1.config) = { sensitive: true }];
}
```

After — `forge.yaml` (or `config.prod.yaml` sibling):

```yaml
environments:
  prod:
    config:
      database_url: "${prod-db-credentials}"   # overrides default secret name
```

After — `deploy/kcl/prod/main.k`:

```kcl
import deploy.kcl.prod.config_gen as cfg
# ...
env_vars = cfg.APP_ENV   # database_url projects to a secretKeyRef automatically
```

`forge generate` writes `deploy/kcl/prod/config_gen.k` with the
matching `secret_ref` shape. The `DB_ENV` lambda in `base.k` can be
deleted once nothing references it.

## 5. Verification

```bash
# Sensitive fields project to secret_ref, not inline values.
grep -r "DATABASE_URL\|STRIPE_API_KEY" deploy/kcl/
# Expect: only secret_ref lines (no `value = "<plaintext>"` for sensitive fields).

# `forge run` projects merged config to subprocess env.
forge run --env dev | head    # check the inherited env shows your fields

# Build / deploy still work.
forge generate && go build ./...
forge deploy dev --dry-run    # KCL renders cleanly
```

If the audit flags a remaining `<NAME>_ENV` reference in `base.k`,
that's a missed cleanup step — drop it once nothing imports it.

## 6. Rollback

If the new shape breaks something:

```bash
git checkout HEAD -- forge.yaml proto/config/v1/config.proto deploy/kcl/
forge upgrade --to <prior-version>
```

`--to <prior-version>` requires the older forge build on PATH. The
hand-curated `<NAME>_ENV` lambdas in `base.k` work unchanged in the
older shape; the proto annotations are additive (field numbers don't
change), so reverting `forge.yaml` and removing the new annotations
is enough to restore the old build flow.

## See also

- `architecture` skill — where per-env config sits in the generate
  pipeline.
- `deploy` skill — how `forge deploy <env>` consumes the rendered
  KCL.
- `proto` skill — `(forge.v1.config) = { sensitive, category }`
  annotation reference.
- `MIGRATION_TIPS.md` "Per-environment config (forge.yaml + sibling
  files + KCL gen)" for the design rationale.
