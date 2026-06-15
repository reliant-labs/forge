"""
Forge KCL — shared base helpers.

Common env-var groups and init-container patterns that most forge
projects layer onto their typed entities. Pure data — no rendering
logic. The render layer (`render.k`) is what walks Bundle and
projects to JSON / k8s manifests.

Schemas like `EnvVar` are declared in `schema.k`. Sibling files in
the same KCL package see them as bare names — no `import` needed.
"""

# Default env-var bundle for accessing a Postgres database via
# a Kubernetes Secret. Projects that use a different secret name
# can copy this and tweak it.
DB_ENV: [EnvVar] = [
    EnvVar {
        name = "DATABASE_URL"
        secret_ref = "db-credentials"
        secret_key = "database-url"
    }
    EnvVar {
        name = "DB_HOST"
        secret_ref = "db-credentials"
        secret_key = "host"
    }
    EnvVar {
        name = "DB_PORT"
        secret_ref = "db-credentials"
        secret_key = "port"
    }
    EnvVar {
        name = "DB_NAME"
        secret_ref = "db-credentials"
        secret_key = "name"
    }
    EnvVar {
        name = "DB_USER"
        secret_ref = "db-credentials"
        secret_key = "username"
    }
    EnvVar {
        name = "DB_PASSWORD"
        secret_ref = "db-credentials"
        secret_key = "password"
    }
]

# OTLP exporter env vars for projects that ship a sidecar / cluster
# OpenTelemetry collector. When unset upstream forge libraries are
# no-ops.
OTEL_ENV: [EnvVar] = [
    EnvVar {
        name = "OTEL_EXPORTER_OTLP_ENDPOINT"
        value = "http://otel-collector.observability:4317"
    }
    EnvVar {
        name = "OTEL_TRACES_SAMPLER"
        value = "parentbased_traceidratio"
    }
    EnvVar {
        name = "OTEL_TRACES_SAMPLER_ARG"
        value = "0.1"
    }
]

# Projects override this in their own files when they need a base
# layer for every entity.
COMMON_ENV: [EnvVar] = []

# Per-env default image tags. Kept centrally so the forge CLI's
# `-D image_tag=...` override has ONE fallback table to consult; the
# scaffold templates no longer re-declare a per-env literal.
_IMAGE_TAG_DEFAULTS: {str: str} = {
    "dev" = "latest"
    "staging" = "staging"
    "prod" = "stable"
}

# image_tag(env) — resolve the image tag for an environment.
#
# Precedence:
#   1. `kcl run -D image_tag=<tag>` (the forge CLI passes this on every
#      deploy / build / dev cluster reload — see internal/cluster.RenderManifests)
#   2. The per-env default from _IMAGE_TAG_DEFAULTS (latest/staging/stable)
#   3. "latest" for unknown envs (so user-defined envs like "preview"
#      still get a usable tag without a forced override)
#
# Usage in a per-env main.k:
#
#     _env = forge.RenderEnv {
#         cluster   = _dev_k8s
#         image_tag = forge.image_tag("dev")
#     }
image_tag = lambda env: str -> str {
    option("image_tag") or _IMAGE_TAG_DEFAULTS[env] or "latest"
}
