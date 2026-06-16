---
name: secrets-file-to-secret-provider
description: Move per-service HostDeploy.secrets_file paths onto a single bundle-level secret_provider. The old shape declared the same gitignored dotenv on every HostDeploy and only covered host services; the new shape declares the provider once on the Bundle so host, compose, and k8s all draw secrets from one source, fail-fast on missing refs, and get a real prod story via ExternalSecrets.
applies-from: v0.7.0
applies-to: v0.8.0
detection: grep -l "secrets_file" deploy/kcl/
relevance: migration
---

# Migrating HostDeploy secrets_file → bundle secret_provider

Use this skill when the project's `deploy/kcl/<env>/main.k` declares one
or more `forge.HostDeploy { secrets_file = "..." }` blocks and you want a
single secret source across host, compose, and k8s — or when `forge
upgrade` reports a jump that crosses v0.8.0.

## 1. What changed

v0.7 attached secrets to each host service via a per-service
`HostDeploy.secrets_file` path. That had three limits:

  * **Host-only.** It fed host processes but said nothing about how
    compose or k8s services got the same secrets — those drifted onto
    ad-hoc `env_file` overlays or hand-maintained cluster Secrets.
  * **Repeated.** Every HostDeploy block repeated the same
    `.env.dev.secrets` path; nothing tied them to one source of truth.
  * **No prod story.** A committed-path-to-a-dotenv shape has no answer
    for staging/prod, where forge must never see the values.

v0.8+ declares secrets once on the `Bundle`. A secret is still declared
as a *reference* in KCL `env_vars` (`forge.EnvVar { secret_ref, secret_key }`);
its **value** comes from the bundle's per-env provider:

```kcl
schema Bundle:
    services: [Service]
    secret_provider?: DotenvSecrets | ExternalSecrets   # per-env value source
```

  * `forge.DotenvSecrets { path = ".env.dev.secrets" }` (dev / local) —
    forge reads the gitignored dotenv (keyed by env-var NAME) and injects
    it per runtime: host gets the whole dotenv as its secrets layer,
    compose/external get it merged under the `env_file` overlay, k8s
    renders Secret objects from the declared cluster `secret_ref`s and
    applies them before Deployments (guarded to LOCAL clusters only).
    `forge up` fail-fasts if a declared `secret_ref` is missing from the
    dotenv.
  * `forge.ExternalSecrets {}` (staging / prod) — forge never sees the
    values. k8s references pre-existing Secrets (ESO / sealed), host and
    external self-fetch. Inert on forge's side; no other fields exist.

The per-service `HostDeploy.secrets_file` still works as a backward-compat
fallback, but it no longer drives compose or k8s.

## 2. Detection

```bash
# Old shape: secrets_file on HostDeploy blocks.
grep -rn "secrets_file" deploy/kcl/ 2>/dev/null

# New shape: secret_provider on the Bundle.
grep -rln "secret_provider" deploy/kcl/ 2>/dev/null
```

## 3. Migration (deterministic part)

```bash
# 1. Find every per-service secrets_file path (usually all the same).
grep -rn "secrets_file" deploy/kcl/
```

Worked example — before (`deploy/kcl/dev/main.k`):

```kcl
_bundle = forge.Bundle {
    services = [
        forge.Service {
            name = "admin-server"
            deploy = forge.HostDeploy {
                runner = "air"
                env_vars = _host_env
                secrets_file = ".env.dev.secrets"
            }
        }
        forge.Service {
            name = "billing-server"
            deploy = forge.HostDeploy {
                runner = "go-run"
                env_vars = _host_env
                secrets_file = ".env.dev.secrets"
            }
        }
    ]
}
```

After — one provider on the bundle, no per-service `secrets_file`:

```kcl
_bundle = forge.Bundle {
    services = [
        forge.Service {
            name = "admin-server"
            deploy = forge.HostDeploy {
                runner = "air"
                env_vars = _host_env
            }
        }
        forge.Service {
            name = "billing-server"
            deploy = forge.HostDeploy {
                runner = "go-run"
                env_vars = _host_env
            }
        }
    ]
    # One source for host + compose + k8s in dev.
    secret_provider = forge.DotenvSecrets { path = ".env.dev.secrets" }
}
```

And in `deploy/kcl/prod/main.k` (forge never sees the values):

```kcl
_bundle = forge.Bundle {
    services = [ /* ... all on forge.K8sCluster ... */ ]
    secret_provider = forge.ExternalSecrets {}
}
```

## 4. Migration (manual part)

What user code / config might need to change:

- **Declare each secret as a ref.** A value only flows if some
  `env_vars` entry references it as
  `forge.EnvVar { secret_ref = "...", secret_key = "..." }`. Audit your
  dotenv keys and add a ref for each one a service consumes — the
  provider supplies the value, the ref names it.
- **Drop the repeated `secrets_file` lines.** Remove them from every
  HostDeploy once the bundle provider is in place. (Leaving one in place
  is harmless — it's a fallback — but it's dead weight.)
- **Cluster secret_refs for k8s.** In dev, `DotenvSecrets` renders Secret
  objects only for the `secret_ref`s the cluster services declare, and
  only into LOCAL clusters. Make sure each cluster service declares the
  refs it needs.
- **Prod Secrets are external.** With `ExternalSecrets {}`, the referenced
  Secrets must already exist in the cluster (ESO / sealed-secrets). Forge
  applies nothing.

## 5. Verification

```bash
# Render KCL and confirm secret_provider is set and no secrets_file remains.
kcl run deploy/kcl/dev --format json -S output \
  | jq '{provider: .secret_provider,
         stray_secrets_file: [.services[] | .deploy.secrets_file] | map(select(. != null))}'

# Host services still get their secrets (set a canary secret_ref, grep the log).
forge up --env=dev
grep -i CANARY .forge/logs/dev/*.log

# Fail-fast: delete a declared key from the dotenv and confirm forge up errors
# naming the missing secret_ref (then restore it).

# k8s: confirm Secret objects render only into the local cluster, applied
# before Deployments (kubectl get secret -n <dev-ns>); none in staging/prod.
```

## 6. Rollback

If the new shape breaks something:

```bash
# Restore the per-service secrets_file shape from git.
git checkout HEAD -- deploy/kcl/

# Downgrade.
forge upgrade --to <prior-version>
```

The per-service `secrets_file` fallback still loads the same dotenv, so
reverting the KCL restores the v0.7 host loop unchanged.

## Gotcha — config is not a secret

Only true secret *values* belong in the provider. Anything that merely
**varies by runtime** (DATABASE_URL, NATS_URL, PORT, LOG_LEVEL, public
identifiers) is config: keep it in KCL `env_vars`, where it's reproducible
and version-controlled. Pushing config through the dotenv re-introduces
the silent per-machine drift the env_vars split was meant to kill.

## See also

- `forge/secrets` skill — the full secret-provider model (references,
  per-runtime injection, fail-fast, ExternalSecrets).
- `host-env-file-to-env-vars` skill — the prior migration that split
  `env_file` into `env_vars` + `secrets_file`; this skill moves the
  secrets half it produced onto the bundle provider.
- `auth` skill — how secrets project into Kubernetes Secrets for cluster
  mode.
