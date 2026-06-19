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

A secret reference is a `forge.EnvVar` carrying `secret_ref` (+ optional
`secret_key`, which defaults to `name`). It already projects to a k8s
`secretKeyRef`. For the HOST runtime the dotenv key is `EnvVar.name`.

```kcl
# A service's env_vars — same declaration in every env
forge.EnvVar { name = "STRIPE_SECRET_KEY", secret_ref = "app-secrets" }
forge.EnvVar { name = "JWT_SIGNING_KEY",  secret_ref = "app-secrets", secret_key = "jwt-signing-key" }
```

```bash
# .env.dev.secrets  (gitignored) — keyed by env-var NAME
STRIPE_SECRET_KEY=sk_test_...
JWT_SIGNING_KEY=base64-private-key...
```

## Per-env provider

Set `Bundle.secret_provider` per env. It picks where forge gets values.

```kcl
# deploy/kcl/dev/main.k — DEV pulls from a gitignored dotenv
_bundle = forge.Bundle {
    secret_provider = forge.DotenvSecrets { path = ".env.dev.secrets" }
    services = [ ... ]
}
```

```kcl
# deploy/kcl/prod/main.k — PROD: values live outside forge's view
_bundle = forge.Bundle {
    secret_provider = forge.ExternalSecrets {}
    services = [ ... ]
}
```

- `forge.DotenvSecrets { path = ".env.dev.secrets" }` — `type="dotenv"`;
  has a `path`. DEV / LOCAL only.
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

**Validation:** `forge up` / `forge deploy` **fail-fast** if a declared
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

- **CONFIG** — values that **vary by runtime** (host
  `DATABASE_URL=localhost:5434` vs in-cluster DNS). These stay in KCL
  `env_vars`, rebound per runtime. NOT secrets.
- **SECRET** — true secret **values** that are **identical across
  runtimes** (API keys, tokens, signing keys). These come from the
  provider.

For a secret embedded in a URL, **split it**: the password is a secret
(provider), the URL template is config (KCL `env_vars`), composed via
`${...}`:

```kcl
forge.EnvVar { name = "DB_PASSWORD",  secret_ref = "app-secrets" }              # secret → provider
forge.EnvVar { name = "DATABASE_URL", value = "postgres://app:${DB_PASSWORD}@db:5432/app" }  # config → KCL
```

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
  in the dotenv aborts `forge up` / `forge deploy` — no more silent
  feature-disable.
- **Dotenv is keyed by env-var NAME.** The dotenv key must match
  `EnvVar.name` (not `secret_ref` / `secret_key`).
- **ExternalSecrets is inert.** It renders nothing and validates
  nothing — it only declares that values live outside forge. It has
  **no** provider/auth fields (no aws-secrets-manager / vault keys).
- **env_file wins.** Under compose/external, an explicit `env_file`
  overlay overrides the dotenv-provided values.
