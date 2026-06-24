"""
Positive test — env-wide DIGEST deploy reference.

`forge deploy` pins the manifest to an immutable content-addressed digest
when the build state captured one: it threads the digest into the env-wide
RenderEnv.image_tag as the `@sha256:...` form. The _image_ref seam then
appends it with NO `:` separator → `<registry>/<image>@sha256:...`, dropping
the env tag. A digest can't go stale or be re-pointed, so the node-cache /
re-tag-didn't-take failure class is impossible.

Invariants:
  * An un-pinned forge-built image with an env-wide DIGEST image_tag renders
    `<registry>/<image>@sha256:...` (no colon, digest appended).
  * A service with its OWN plain image_tag still wins (per-service pin),
    rendered as `:tag` — the env-wide digest does not override it.
  * A third-party image that already carries its OWN `:tag` passes through
    verbatim — the env-wide digest is ignored for it (registry prefixed).
  * imagePullPolicy for a digest ref is IfNotPresent (immutable → cacheable,
    NOT the dirty/latest Always path).
"""

import forge

_digest = "@sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"

# Un-pinned forge-built image — takes the env-wide digest.
_built_svc = forge.Service {
    name = "control-plane"
    image = "control-plane"
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "ghcr.io/reliant"
    }
}

# Per-service plain pin — wins over the env-wide digest.
_pinned_svc = forge.Service {
    name = "sidecar"
    image = "sidecar"
    image_tag = "v9.9.9"
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "ghcr.io/reliant"
    }
}

# Third-party image with its own :tag — env-wide digest ignored.
_vendor_svc = forge.Service {
    name = "litellm"
    image = "ghcr.io/berriai/litellm:main-v1.0"
    deploy = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "ghcr.io/reliant"
    }
}

_bundle = forge.Bundle {
    services = [_built_svc, _pinned_svc, _vendor_svc]
}

# The env-wide image_tag is a DIGEST — exactly what forge deploy emits when
# the build state captured a pushed digest.
_env = forge.RenderEnv {
    cluster = forge.K8sCluster {
        cluster = "k3d-dev"
        namespace = "dev"
        registry = "ghcr.io/reliant"
    }
    image_tag = _digest
    network_policies = False
}

manifests = forge.render_manifests(_bundle, _env)

_container_of = lambda n: str -> any {
    [m for m in manifests if m.metadata.name == n and "spec" in m \
        and (m.kind == "Deployment" or m.kind == "Job")][0].spec.template.spec.containers[0]
}
_image_of = lambda n: str -> str {
    _container_of(n).image
}

# ── Invariants ─────────────────────────────────────────────────────

# Un-pinned forge-built image renders the env-wide digest, no colon.
assert_built_digest = _image_of("control-plane") == \
    "ghcr.io/reliant/control-plane@sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"

# Per-service plain pin wins over the env-wide digest.
assert_pinned_wins = _image_of("sidecar") == "ghcr.io/reliant/sidecar:v9.9.9"

# Third-party :tag image passes through verbatim (registry prefixed).
assert_vendor_passthrough = _image_of("litellm") == \
    "ghcr.io/reliant/ghcr.io/berriai/litellm:main-v1.0"

# A digest ref is immutable → IfNotPresent (NOT the dirty/latest Always path).
assert_digest_pull_policy = _container_of("control-plane").imagePullPolicy == "IfNotPresent"
