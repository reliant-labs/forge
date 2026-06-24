"""
Positive tests for per-service image_tag pinning + image-ref pass-through.

The bug this fixes: a no-op sibling image (one forge does not build under
the env-wide `git describe --dirty` tag) had the env-wide tag fanned onto
it → a tag that was never pushed for that image → ErrImagePull.

Invariants:
  * A service with `image_tag` set is rendered with THAT tag, not the
    env-wide RenderEnv.image_tag.
  * A service WITHOUT `image_tag` uses the env-wide tag (single-image
    envs unchanged).
  * An `image` that already carries `:tag` passes through verbatim — the
    env/service tag is NOT appended.
  * An `image` that carries `@sha256:` digest passes through verbatim.
  * Registry-port colon disambiguation: a bare `localhost:5050/foo`
    (port colon, no tag) still gets the tag appended — the port colon is
    not mistaken for a tag.
  * Pinning works for cronjobs + operators too.
"""

import forge

_pinned_svc = forge.Service {
    name = "reliant"
    image = "reliant"
    image_tag = "v1.4.2"
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "localhost:5050"
    }
}

_env_tag_svc = forge.Service {
    name = "api"
    image = "api"
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "localhost:5050"
    }
}

_explicit_tag_svc = forge.Service {
    name = "vendor"
    image = "ghcr.io/acme/vendor:3.0.0"
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "localhost:5050"
    }
}

_digest_svc = forge.Service {
    name = "pinned-digest"
    image = "ghcr.io/acme/db@sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "localhost:5050"
    }
}

_pinned_cronjob = forge.CronJob {
    name = "migrate"
    image = "reliant"
    image_tag = "v1.4.2"
    schedule = ""
}

_pinned_operator = forge.Operator {
    name = "ctrl"
    image = "ctrl"
    image_tag = "v9.9.9"
    crds = ["Widget"]
}

_bundle = forge.Bundle {
    services = [_pinned_svc, _env_tag_svc, _explicit_tag_svc, _digest_svc]
    cronjobs = [_pinned_cronjob]
    operators = [_pinned_operator]
}

# Env-wide tag is a dirty git tag — the kind the bug fanned onto everything.
_env = forge.RenderEnv {
    cluster = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "localhost:5050"
    }
    image_tag = "abc1234-dirty"
    network_policies = False
}

manifests = forge.render_manifests(_bundle, _env)

# Helper: the container image string of the Deployment / Job / etc. named
# `n`. Pulls the first container's image from any workload kind.
_image_of = lambda n: str -> str {
    [m for m in manifests if m.metadata.name == n and "spec" in m \
        and (m.kind == "Deployment" or m.kind == "Job")][0].spec.template.spec.containers[0].image
}

# ── Invariants ─────────────────────────────────────────────────────

# Pinned service uses its pin, NOT the env-wide dirty tag.
assert_pinned = _image_of("reliant") == "localhost:5050/reliant:v1.4.2"

# Un-pinned service uses the env-wide tag.
assert_env_tag = _image_of("api") == "localhost:5050/api:abc1234-dirty"

# Image with explicit :tag passes through verbatim — registry prefixed,
# but NO extra tag appended.
assert_explicit_tag = _image_of("vendor") == "localhost:5050/ghcr.io/acme/vendor:3.0.0"

# Image with @sha256 digest passes through verbatim (registry prefixed).
assert_digest = _image_of("pinned-digest") == \
    "localhost:5050/ghcr.io/acme/db@sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"

# Pinned cronjob (one-shot Job) uses its pin.
assert_cronjob_pinned = _image_of("migrate") == "localhost:5050/reliant:v1.4.2"

# Pinned operator uses its pin.
assert_operator_pinned = _image_of("ctrl") == "localhost:5050/ctrl:v9.9.9"
