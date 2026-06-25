"""
Forge KCL — shared base helpers.

Common env-var groups and init-container patterns that most forge
projects layer onto their typed entities. Pure data — no rendering
logic. The render layer (`render.k`) is what walks Bundle and
projects to JSON / k8s manifests.

Schemas like `EnvVar` are declared in `schema.k`. Sibling files in
the same KCL package see them as bare names — no `import` needed.
"""

import json

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

# env_merge(base, overrides) — combine two EnvVar lists with name-keyed,
# OVERRIDE-WINS semantics. This is the explicit, public replacement for the
# `base + overrides` KCL list-concatenation footgun.
#
# Why this exists (the footgun it kills)
# ======================================
# KCL list `+` CONCATENATES — it does NOT de-duplicate. So
# `cfg.APP_ENV + [EnvVar { name = "LOG_LEVEL", value = "debug" }]` where
# APP_ENV already carries a LOG_LEVEL emits the variable TWICE, and
# kubectl server-side apply REJECTS the Deployment ("duplicate entries for
# key [name=...]"). Today that is silently rescued by a HIDDEN
# `_dedup_env_vars` the render layer applies downstream — an implicit
# contract an author writing a new env can't see. `env_merge` makes the
# "later wins on name conflict" rule VISIBLE on the public `forge` surface,
# so a reader (human or LLM) can author a correct override without knowing
# the renderer's secret.
#
# Semantics (matches the render-time dedup exactly, so it is
# render-equivalent to `base + overrides`):
#   * one entry per `name`;
#   * on a name conflict the LATER occurrence wins — an `overrides` entry
#     supersedes a `base` entry, exactly the way the host-mode env layering
#     and the K8sCluster render-time dedup already resolve it;
#   * surviving entries keep their LAST-occurrence position (the same order
#     the renderer's dedup produced), so the rendered container `env` block
#     is byte-identical before/after migrating a `+` chain to `env_merge`.
#
# It is variadic-by-nesting: chain `env_merge(env_merge(a, b), c)` to layer
# more than two sources, left-to-right, last-wins — the direct analogue of
# `a + b + c` but de-duplicated and explicit.
#
# Example:
#   _env = forge.env_merge(cfg.APP_ENV, [
#       forge.EnvVar { name = "LOG_LEVEL", value = "debug" }  # wins over APP_ENV's
#   ])
env_merge = lambda base: [EnvVar], overrides: [EnvVar] -> [EnvVar] {
    _all = base + overrides
    [
        _all[i] for i in range(len(_all))
        # keep this entry only if no LATER entry shares its name (last-wins)
        if len([1 for j in range(i + 1, len(_all)) if _all[j].name == _all[i].name]) == 0
    ]
}

# ---------------------------------------------------------------------------
# Standard `-D <key>=<value>` render options.
# ---------------------------------------------------------------------------
#
# Every render the forge CLI drives passes a STANDARD set of top-level KCL
# bindings via `kcl run -D <key>=<value>` (see
# internal/cluster.RenderManifests / renderDArgs). They are how the deploy /
# build / dev-cluster-reload pass per-invocation facts INTO your main.k. KCL
# surfaces them through the `option("<key>")` builtin; the typed accessors
# below wrap each one with a default + doc so a fresh reader can DISCOVER the
# whole set from the `forge` surface instead of copying a sibling env.
#
# The standard keys, ALWAYS passed:
#
#   env          -> the environment name ("dev" / "staging" / "prod" / …).
#                   forge.env(). Use for per-env conditional manifests
#                   (see forge.env() / README "Per-env conditional manifests").
#   image_tag    -> the resolved image tag for the env. forge.image_tag(env)
#                   (declared below; precedence: -D override > per-env
#                   default > "latest").
#   namespace    -> the k8s namespace the manifests deploy into.
#                   forge.namespace(default).
#
# Passed CONDITIONALLY (only when forge has a value to bind):
#
#   image_digests -> JSON name→digest map `forge deploy` pins so each image
#                   resolves to ITS content-addressed digest. forge.image_digests().
#                   Absent on the local-registry / `kcl run` path (→ {}).
#   registry      -> the image registry. Note: the forge CLI does NOT pass
#                   this by default — your main.k owns the per-env registry
#                   value (a literal or `forge.registry(default)`); the option
#                   exists so a one-off `kcl run -D registry=…` can override it.
#
# Plus EVERY non-sensitive per-env config field (from forge.yaml + the config
# proto) is passed as its own `-D <FIELD>=<value>` and surfaced through the
# generated `deploy/kcl/<env>/config_gen.k` (cfg.APP_ENV) — you read those via
# `cfg.<...>`, not raw `option()`.

# env() — the current environment name (the `-D env=<name>` binding the forge
# CLI passes on every render). `default` is used for a plain `kcl run` with no
# binding. Use for per-env conditional manifests:
#
#     _is_dev_host = forge.env() == "dev-host"
#     additional_manifests = [] if _is_dev_host else [ ...in-cluster infra... ]
env = lambda default: str = "dev" -> str {
    option("env") or default
}

# namespace(default) — the k8s namespace the render deploys into (the
# `-D namespace=<ns>` binding the forge CLI passes on every cluster render).
# `default` is the per-env literal your main.k falls back to for a plain
# `kcl run`.
namespace = lambda default: str -> str {
    option("namespace") or default
}

# registry(default) — the image registry. The forge CLI does NOT pass
# `-D registry=` by default (the per-env registry is yours to declare), so
# this exists for the one-off override `kcl run -D registry=<reg>`; `default`
# is your per-env registry literal.
registry = lambda default: str -> str {
    option("registry") or default
}

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
#     manifests = forge.render_manifests(_bundle, forge.image_tag("dev"))
image_tag = lambda env: str -> str {
    option("image_tag") or _IMAGE_TAG_DEFAULTS[env] or "latest"
}

# image_digests() — the per-image content-addressed digest map forge deploy
# passes on the command line.
#
# forge deploy resolves each built image's pushed manifest digest from the
# build state and passes the map as a JSON string:
#   `kcl run -D image_digests='{"reliant":"sha256:...","control-plane":"sha256:..."}'`
# keyed on the BARE image name. This helper decodes it to `{str:str}` so a
# per-env main.k can thread it into `forge.render_manifests`'s
# `image_digests` param (which pins each service's manifest image to ITS
# image's digest) AND consult it for any hand-composed image reference
# (e.g. a DAEMON_IMAGE env var that names workspace-base directly).
#
# Empty / unset (the local-registry e2e path and a plain `kcl run` with no
# digest binding) returns `{}`, so every image stays on its tag — unchanged.
#
#     manifests = forge.render_manifests(
#         _bundle, _image_tag, forge.image_digests())
image_digests = lambda -> {str:str} {
    _raw = option("image_digests") or ""
    json.decode(_raw) if _raw else {}
}

# image_ref_for(registry, image, tag) — build an image reference that
# prefers the per-image digest from image_digests() when one was captured,
# else falls back to `<registry>/<image>:<tag>`. The companion to
# forge.image_digests() for hand-composed references that don't flow through
# the K8sCluster service renderer — chiefly an env var that names an image
# by hand (DAEMON_IMAGE = the workspace-base ref a controller reads at
# runtime). When the image has a digest, emits `<registry>/<image>@sha256:…`
# (immutable); otherwise `<registry>/<image>:<tag>`, byte-identical to the
# manual string it replaces.
image_ref_for = lambda registry: str, image: str, tag: str -> str {
    _d = image_digests()
    _digest = _d[image] if (image in _d) else ""
    "${registry}/${image}@${_digest}" if _digest else "${registry}/${image}:${tag}"
}
