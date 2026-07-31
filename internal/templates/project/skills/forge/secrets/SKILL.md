---
name: secrets
description: The forge secret-provider model — declare a secret once as a reference, bind its value per-env via a provider (dotenv for dev/local, external for prod/staging), and let forge resolve + inject per runtime. KCL stays pure — no secret values ever in rendered output.
---

# Secrets

## Mental model

A secret is **declared once** as a *reference* (an `EnvVar` with a
`secret_ref`); its **value** comes from a **per-env provider** set on the
bundle. Forge resolves the value once and injects it per runtime
(host / compose / external / k8s). KCL stays pure — secret values never
appear in rendered KCL output, only the references do. Dev/local pulls
values from a gitignored dotenv; prod/staging declares that the values
live somewhere forge never sees (External Secrets Operator, sealed
secrets, workload identity).

## Declare (once, env-invariant)

### The normal path: a `sensitive` config field

App config lives in `proto/config/v1/config.proto`. Marking a field
`sensitive: true` is the whole declaration — forge does the rest:

```proto
string database_url = 3 [(forge.v1.config) = {
  env_var: "DATABASE_URL",
  required: true,
  sensitive: true,
  description: "PostgreSQL connection string."
}];
```

What that annotation buys, in every environment:

- `deploy/kcl/config_schema.k` types the field as a `ConfigSecretRef`
  (a Secret **name + key**), defaulted to
  `{name = "<project>-secrets", key = "<env_var lowercased>"}`.
- `deploy/kcl/config_projection.k` projects it as `from_secret`, so the
  rendered manifest carries `valueFrom.secretKeyRef` — **never** an
  inline `value:`.
- The per-env `deploy/kcl/<env>/config.k` (git-tracked) carries
  **nothing** for it. There is no slot to accidentally paste a password
  into.
- `pkg/config` registers **no CLI flag** for it (a secret must not land
  in shell history) and never echoes it.

Point it at a different Secret by writing the override in `config.k`:

```kcl
app_config: config_schema.AppConfig = {
    database_url = config_schema.ConfigSecretRef {name = "rds-creds", key = "url"}
}
```

`forge doctor --signal deploy` fails the **Deploy Secrets** check when
any credential-shaped env var renders as a literal — that check going
green is what the annotation is for.

### The escape hatch: a raw `EnvVar` secret_ref

For a secret that is NOT app config (a sidecar's token, an infra
service's password), declare the reference directly. A secret reference
is a `forge.EnvVar` carrying `secret_ref` (+ optional `secret_key`,
which defaults to `name`). It projects to the same k8s `secretKeyRef`.
For the HOST runtime the dotenv key is `EnvVar.name`.

```kcl
# A service's env_vars — same declaration in every env
forge.EnvVar { name = "STRIPE_SECRET_KEY", secret_ref = "app-secrets" }
forge.EnvVar { name = "JWT_SIGNING_KEY",  secret_ref = "app-secrets", secret_key = "jwt-signing-key" }
```

```bash
# .env.dev  (gitignored) — keyed by env-var NAME
DATABASE_URL=postgres://postgres:postgres@localhost:5434/myapp?sslmode=disable
STRIPE_SECRET_KEY=sk_test_...
JWT_SIGNING_KEY=base64-private-key...
```

## Per-env provider

Set `Bundle.secret_provider` per env. It picks where forge gets values.

```kcl
# deploy/kcl/dev/main.k — DEV pulls from a gitignored dotenv
_bundle = forge.Bundle {
    secret_provider = forge.DotenvSecrets { path = ".env.dev" }
    services = [ ... ]
}
```

```kcl
# deploy/kcl/prod/main.k — PROD: values live outside forge's view
_bundle = forge.Bundle {
    secret_provider = forge.ExternalSecrets {}
    # Declare WHAT must exist out of band, so a missing Secret BLOCKS the
    # deploy instead of surfacing as CreateContainerConfigError minutes later.
    required_secrets = [forge.ExternalSecret {
        name = "myapp-secrets"
        namespace = "myapp-prod"
        keys = ["database_url"]
        reason = "backs every `sensitive` config field"
    }]
    services = [ ... ]
}
```

- `forge.DotenvSecrets { path = ".env.dev" }` — `type="dotenv"`;
  has a `path`. DEV / LOCAL only. `.env.<env>` is the convention forge
  scaffolds and every value-resolving path reads.
- `forge.ExternalSecrets {}` — `type="external"`; a pure marker, **no
  other fields**. PROD / STAGING.

## Per-runtime: what forge does

With **DotenvSecrets**, forge reads the dotenv (keyed by env-var name)
and injects it differently per runtime:

| Runtime | What forge does |
|---|---|
| host / air | The whole dotenv becomes the secrets layer (provider-first). Per-service `HostDeploy.secrets_file` is now only a backward-compat fallback when no bundle provider is declared. |
| compose / external | Dotenv is merged **under** the `env_file` overlay — an explicit `env_file` wins. |
| k8s | Forge **renders** Secret objects CLI-side from the declared cluster `secret_ref`s and `kubectl apply`s them **before** the Deployments, so `secretKeyRef` resolves. Guarded by an `isLocalCluster` check — forge **refuses** to render plaintext into a non-local cluster (only k3d / kind / docker-desktop / minikube / rancher-desktop / colima / orbstack). |

**Validation:** `forge env up` / `forge env deploy` **fail-fast** if a declared
`secret_ref` has no value in the dotenv. (This replaces the old silent
"missing secret → feature disabled" behavior.)

With **ExternalSecrets**, forge **never sees values** and is inert on
its side — it renders nothing and validates nothing. k8s references
pre-existing Secrets (External Secrets Operator / sealed); host &
external runtimes self-fetch (workload identity / ambient env). The
marker just makes the contract explicit.

## Config vs secret — the split

Keep these straight or you'll leak runtime-specific values into the
provider:

- **CONFIG** — values that carry no credential (`LOG_LEVEL`,
  `PORT`, an OTLP endpoint). These project inline, and vary per env via
  `deploy/kcl/<env>/config.k`.
- **SECRET** — anything that carries a credential: a password, a token,
  a key, **or a URL with credentials embedded in it**. These project as
  a Secret reference and get their value from the provider.

A connection string **is a secret** — do not try to split it into a
non-secret template plus a secret password. The value that varies per
env and the value that must stay out of git are the SAME string here, so
splitting buys nothing and costs a fragile interpolation: k8s expands
`$(VAR)` (not `${VAR}`), only from vars defined **earlier in the same
container's env list**, so the composed URL silently renders with a
literal `${DB_PASSWORD}` in it more often than it works. Mark the whole
field `sensitive` and give each env its own Secret value.

## Supersedes

The per-service `HostDeploy.secrets_file` dotenv. It still works as a
**host fallback** when no bundle `secret_provider` is declared, but the
bundle provider is the single-source model now — prefer it.

## Gotchas

- **Dotenv is local-only.** The `isLocalCluster` guard makes forge
  refuse to render plaintext Secrets into anything but a local cluster
  (k3d / kind / docker-desktop / minikube / rancher-desktop / colima /
  orbstack). Use `ExternalSecrets` for staging/prod.
- **Fail-fast on missing refs.** A declared `secret_ref` with no value
  in the dotenv aborts `forge env up` / `forge env deploy` — no more silent
  feature-disable.
- **Dotenv is keyed by env-var NAME.** The dotenv key must match
  `EnvVar.name` (not `secret_ref` / `secret_key`).
- **ExternalSecrets is inert.** It renders nothing and validates
  nothing — it only declares that values live outside forge. It has
  **no** provider/auth fields (no aws-secrets-manager / vault keys).
- **env_file wins.** Under compose/external, an explicit `env_file`
  overlay overrides the dotenv-provided values.
