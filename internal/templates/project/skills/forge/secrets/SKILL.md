---
name: secrets
description: The forge secret-provider model — declare a secret once as a reference, bind its value per-env via a provider (a gitignored YAML secret store for dev/local, external for prod/staging), and let forge resolve + inject it into the services that declare it. forge projects use no .env files. KCL stays pure — no secret values ever in rendered output.
---

# Secrets

## Mental model

A secret is **declared once** as a *reference* (an `EnvVar` with a
`secret_ref`); its **value** comes from a **per-env provider** set on the
bundle. Forge resolves the value once and injects it per runtime
(host / compose / external / k8s). KCL stays pure — secret values never
appear in rendered KCL output, only the references do. Dev/local pulls
values from a gitignored YAML secret store (name -> value);
prod/staging declares that the values live somewhere forge never sees
(External Secrets Operator, sealed secrets, workload identity).

**forge projects contain no `.env` files.** `forge lint` fails on one —
see "Why a directory, not a `.env` file" below.

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

- `deploy/kcl/config_gen.k` types the field as a `ConfigSecretRef`
  (a Secret **name + key**), defaulted to
  `{name = "<project>-secrets", key = "<env_var lowercased>"}`.
- The same file's `appConfigEnvMap` projects it as `from_secret`, so the
  rendered manifest carries `valueFrom.secretKeyRef` — **never** an
  inline `value:`.
- The per-env `deploy/kcl/<env>/config.k` (git-tracked) carries
  **nothing** for it. There is no slot to accidentally paste a password
  into.
- `pkg/config` registers **no CLI flag** for it (a secret must not land
  in shell history) and never echoes it.

Point it at a different Secret by writing the override in `config.k`:

```kcl
app_config: config_gen.AppConfig = {
    database_url = config_gen.ConfigSecretRef {name = "rds-creds", key = "url"}
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
For the HOST runtime the store key is `EnvVar.name`.

```kcl
# A service's env_vars — same declaration in every env
forge.EnvVar { name = "STRIPE_SECRET_KEY", secret_ref = "app-secrets" }
forge.EnvVar { name = "JWT_SIGNING_KEY",  secret_ref = "app-secrets", secret_key = "jwt-signing-key" }
```

**Declaring is what makes a value live.** A service receives ONLY the
keys it declares — a value sitting in the store that no service
references reaches nothing. That is deliberate: it means the store
cannot become a side channel for configuration.

```bash
# Store the values (or hand-edit the YAML)
printf '%s' "$STRIPE_KEY" | forge secret set dev STRIPE_SECRET_KEY
forge secret set dev JWT_SIGNING_KEY --from-file ./key.pem

forge secret ensure dev   # create the dir + list refs with no value yet
forge secret list dev     # names + presence, never values
```

## Per-env provider

Set `Bundle.secret_provider` per env. It picks where forge gets values.

```kcl
# deploy/kcl/dev/main.k — DEV reads a gitignored directory
_bundle = forge.Bundle {
    secret_provider = forge.FileSecrets { path = "secrets/dev.yaml" }
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

- `forge.FileSecrets { path = "secrets/dev.yaml" }` — `type="file"`; a
  gitignored YAML file mapping env-var name to value. DEV / E2E only;
  forge rejects it in any other env. This is what the scaffold emits.
- `forge.ExternalSecrets {}` — `type="external"`; a pure marker, **no
  other fields**. PROD / STAGING.
- `forge.DotenvSecrets { path = ... }` — **DEPRECATED**. Still resolved
  so existing projects run; `forge lint` fails on the file and
  `forge secret migrate <env>` converts it.

## Per-runtime: what forge does

With **FileSecrets**, forge reads the YAML store (env-var name -> value)
and injects it differently per runtime:

| Runtime | What forge does |
|---|---|
| host / air | Each service gets the keys **it declares** via `secret_ref` — not the whole store. Per-service `HostDeploy.secrets_file` is only a backward-compat fallback when no bundle provider is declared. |
| compose / external | Values are merged **under** the `env_file` overlay — an explicit `env_file` wins. |
| k8s | Forge **renders** Secret objects CLI-side from the declared cluster `secret_ref`s and `kubectl apply`s them **before** the Deployments, so `secretKeyRef` resolves. Guarded by an `isLocalCluster` check — forge **refuses** to render plaintext into a non-local cluster (only k3d / kind / docker-desktop / minikube / rancher-desktop / colima / orbstack). |

**Validation:** `forge env up` / `forge env deploy` **fail-fast** if a declared
`secret_ref` has no value in the store, listing every miss with the
`forge secret set` line that fixes it.

## Why a YAML store, not a `.env` file

forge projects do not use `.env` files, and `forge lint` fails on one.

A dotenv was handed to every host service **wholesale**, so adding a line
to it made that value live everywhere *without touching KCL*. That made
the untracked file the cheapest place to put anything — and the result
was predictable: service URLs, client IDs and issuer names migrated out
of version-controlled KCL into a file nobody could review, diff, or
reproduce on another machine.

Two properties fix that, and both matter:

1. **Declaration-scoped injection.** A value reaches only the services
   that declare it, so putting config in the store accomplishes nothing —
   this is the property that actually closed the hole.
2. **YAML, not `KEY=value`.** Real quoting and multi-line values, so a PEM
   key or a JSON blob round-trips without escaping games.

If a value is the same on every developer's machine, it is not a secret:
put it in `deploy/kcl/<env>/config.k`, or declare it as a
`RenderedSecretKey { from = "literal" }` where it stays in git.

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

- **FileSecrets is local-only.** It is rejected outside dev/e2e (a KCL
  check), and the `isLocalCluster` guard makes forge refuse to render
  plaintext Secrets into anything but a local cluster (k3d / kind /
  docker-desktop / minikube / rancher-desktop / colima / orbstack). Use
  `ExternalSecrets` for staging/prod.
- **Fail-fast on missing refs.** A declared `secret_ref` with no value
  in the store aborts `forge env up` / `forge env deploy` — no more silent
  feature-disable.
- **The store is keyed by env-var NAME.** The YAML KEY must match
  `EnvVar.name` (not `secret_ref` / `secret_key`), and must be a valid
  env-var name — forge refuses to load a store containing a key that
  could never be injected.
- **Multi-line values round-trip.** A PEM key or JSON blob is written as
  a YAML block scalar; values with colons or leading spaces are quoted.
- **Undeclared values are inert.** A key no service declares is never
  injected. `forge secret list <env>` reports these — they are usually a
  typo, or config that belongs in `config.k`.
- **ExternalSecrets is inert.** It renders nothing and validates
  nothing — it only declares that values live outside forge. It has
  **no** provider/auth fields (no aws-secrets-manager / vault keys).
- **env_file wins.** Under compose/external, an explicit `env_file`
  overlay overrides the provider-supplied values.
