"""
Positive test — PER-IMAGE digest deploy references.

`forge deploy` pins each service's manifest image to ITS OWN
content-addressed digest when the build state captured one. It threads a
PER-IMAGE name→digest map into render_manifests' image_digests (built Go-side from
the per-image build-state files); the _image_ref seam looks up
`image_digests[<image-name>]` and renders `<registry>/<image>@sha256:...`,
dropping the env tag. A digest can't go stale or be re-pointed, so the
node-cache / re-tag-didn't-take failure class is impossible.

REGRESSION (the bug this test guards): the old code threaded ONE env-wide
digest through image_tag, so EVERY service rendered with that single digest
— a `reliant` service pinned to the `control-plane` digest (`manifest
unknown`). This test proves each distinct image resolves to its OWN digest,
and two services SHARING an image both get that image's digest.

Invariants:
  * Two services with DISTINCT images render DISTINCT digests — each its own.
  * Two services SHARING an image both render THAT image's digest.
  * A service whose image has NO entry in the map falls back to the env tag.
  * A per-service plain image_tag pin wins (image absent from digest map).
  * A third-party image that already carries its OWN `:tag` passes through
    verbatim (registry prefixed); the digest map is irrelevant for it.
  * imagePullPolicy for a digest ref is IfNotPresent (immutable → cacheable,
    NOT the dirty/latest Always path).
"""

import forge

_cp_digest = "sha256:d1818c3c2f8a3823f4f5a533a893bd735035fc6b0d79b700aaa8f1d6727e7182"
_reliant_digest = "sha256:12ac5c2e15de35da3c47b8d0d23a68aad80e60dd69467bd0a689a0292f91d4e4"

_target = forge.ClusterTarget {
    cluster = "k3d-dev"
    namespace = "dev"
    registry = "ghcr.io/reliant"
}

# control-plane image — resolves to ITS digest.
_cp_svc = forge.RenderedWorkload {
    name = "control-plane"
    image = "control-plane"
    deploy = _target.deploy
}

# reliant-api-server + reliant-worker SHARE the `reliant` image — both
# resolve to the reliant digest (NOT control-plane's). This is the exact
# regression: distinct image, distinct digest.
_reliant_api_svc = forge.RenderedWorkload {
    name = "reliant-api-server"
    image = "reliant"
    deploy = _target.deploy
}
_reliant_worker_svc = forge.RenderedWorkload {
    name = "reliant-temporal-worker"
    image = "reliant"
    deploy = _target.deploy
}

# An image with NO digest entry — falls back to the env tag.
_nodigest_svc = forge.RenderedWorkload {
    name = "no-digest"
    image = "no-digest"
    deploy = _target.deploy
}

# Per-service plain pin — wins (image absent from digest map anyway).
_pinned_svc = forge.RenderedWorkload {
    name = "sidecar"
    image = "sidecar"
    image_tag = "v9.9.9"
    deploy = _target.deploy
}

# Third-party image with its own :tag — digest map ignored.
_vendor_svc = forge.RenderedWorkload {
    name = "litellm"
    image = "ghcr.io/berriai/litellm:main-v1.0"
    deploy = _target.deploy
}

_bundle = forge.Bundle {
    cluster_target = _target
    services = [_cp_svc, _reliant_api_svc, _reliant_worker_svc, _nodigest_svc, _pinned_svc, _vendor_svc]
}

# The PER-IMAGE digest map — exactly what forge deploy builds from the
# per-image build-state files. control-plane and reliant carry DIFFERENT
# digests; no-digest/sidecar/litellm are absent.
manifests = forge.render_manifests(_bundle, "staging", {
    "control-plane" = _cp_digest
    "reliant" = _reliant_digest
}, False)

_container_of = lambda n: str -> any {
    [m for m in manifests if m.metadata.name == n and "spec" in m \
        and (m.kind == "Deployment" or m.kind == "Job")][0].spec.template.spec.containers[0]
}
_image_of = lambda n: str -> str {
    _container_of(n).image
}

# ── Invariants ─────────────────────────────────────────────────────

# control-plane → its OWN digest.
assert_cp_digest = _image_of("control-plane") == \
    "ghcr.io/reliant/control-plane@" + _cp_digest

# reliant-api-server → the reliant digest (NOT control-plane's). THE
# regression: a distinct image must NOT inherit another image's digest.
assert_reliant_api_digest = _image_of("reliant-api-server") == \
    "ghcr.io/reliant/reliant@" + _reliant_digest

# reliant-temporal-worker SHARES the reliant image → same reliant digest.
assert_reliant_worker_digest = _image_of("reliant-temporal-worker") == \
    "ghcr.io/reliant/reliant@" + _reliant_digest

# The two distinct images render DISTINCT references (not collapsed to one).
assert_distinct = _image_of("control-plane") != _image_of("reliant-api-server")

# An image with no digest entry falls back to the env tag.
assert_nodigest_fallback = _image_of("no-digest") == "ghcr.io/reliant/no-digest:staging"

# Per-service plain pin wins (rendered as :tag).
assert_pinned_wins = _image_of("sidecar") == "ghcr.io/reliant/sidecar:v9.9.9"

# Third-party :tag image passes through verbatim (registry prefixed).
assert_vendor_passthrough = _image_of("litellm") == \
    "ghcr.io/reliant/ghcr.io/berriai/litellm:main-v1.0"

# A digest ref is immutable → IfNotPresent (NOT the dirty/latest Always path).
assert_digest_pull_policy = _container_of("control-plane").imagePullPolicy == "IfNotPresent"
