"""
Positive tests for per-service image_tag pinning + image-ref pass-through.

The bug this fixes: a no-op sibling image (one forge does not build under
the env-wide `git describe --dirty` tag) had the env-wide tag fanned onto
it → a tag that was never pushed for that image → ErrImagePull.

Invariants:
  * A service with `image_tag` set is rendered with THAT tag, not the
    env-wide render_manifests image_tag.
  * A service WITHOUT `image_tag` uses the env-wide tag (single-image
    envs unchanged).
  * An `image` that already carries `:tag` passes through verbatim — the
    env/service tag is NOT appended.
  * An `image` that carries `@sha256:` digest passes through verbatim.
  * An `image` that names its OWN registry host (`ghcr.io/...`) keeps that
    host: the env registry is NOT prefixed onto it. A third-party image is
    pulled from ITS vendor's registry, and prefixing produced unpullable
    refs like `localhost:5050/ghcr.io/acme/vendor:3.0.0` that fail at
    runtime as ImagePullBackOff rather than at render.
  * A tagged image with NO registry host of its own (`vendorapp:3.0.0`)
    still takes the env registry — the passthrough keys on the host, not
    on the presence of a tag.
  * Registry-port colon disambiguation: a bare `localhost:5050/foo`
    (port colon, no tag) still gets the tag appended — the port colon is
    not mistaken for a tag.
  * Pinning works for cronjobs + operators too.
"""

import forge

_target = forge.ClusterTarget {
    cluster = "k3d-dev"
    namespace = "dev"
    registry = "localhost:5050"
}

_pinned_svc = forge.RenderedWorkload {
    name = "reliant"
    image = "reliant"
    image_tag = "v1.4.2"
    deploy = _target.deploy
}

_env_tag_svc = forge.RenderedWorkload {
    name = "api"
    image = "api"
    deploy = _target.deploy
}

_explicit_tag_svc = forge.RenderedWorkload {
    name = "vendor"
    image = "ghcr.io/acme/vendor:3.0.0"
    deploy = _target.deploy
}

_digest_svc = forge.RenderedWorkload {
    name = "pinned-digest"
    image = "ghcr.io/acme/db@sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
    deploy = _target.deploy
}

# A tagged image with NO registry host — the env registry still applies.
_bare_tagged_svc = forge.RenderedWorkload {
    name = "baretag"
    image = "vendorapp:3.0.0"
    deploy = _target.deploy
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
    cluster_target = _target
    services = [_pinned_svc, _env_tag_svc, _explicit_tag_svc, _digest_svc, _bare_tagged_svc]
    cronjobs = [_pinned_cronjob]
    operators = [_pinned_operator]
}

manifests = forge.render_manifests(_bundle, "abc1234-dirty", {}, False)

# Helper: the container image string of the Deployment / Job / etc. named
# `n`. Pulls the first container's image from any workload kind.
#
# A one-shot Job's metadata.name carries a spec hash (`migrate-<hash>`), so
# match it through the stable forge.dev/job-name label as well as by name —
# the same handle `kubectl get job -l forge.dev/job-name=<n>` uses.
_image_of = lambda n: str -> str {
    [m for m in manifests if "spec" in m \
        and (m.kind == "Deployment" or m.kind == "Job") \
        and (m.metadata.name == n or m.metadata.labels["forge.dev/job-name"] == n)][0].spec.template.spec.containers[0].image
}

# ── Invariants ─────────────────────────────────────────────────────

# Pinned service uses its pin, NOT the env-wide dirty tag.
assert_pinned = _image_of("reliant") == "localhost:5050/reliant:v1.4.2"

# Un-pinned service uses the env-wide tag.
assert_env_tag = _image_of("api") == "localhost:5050/api:abc1234-dirty"

# Image with explicit :tag AND its own registry host passes through fully
# verbatim: no extra tag appended, and NO env registry prefixed. Prefixing
# here yielded `localhost:5050/ghcr.io/acme/vendor:3.0.0`, a ref no registry
# can serve — the failure surfaced as ImagePullBackOff in a live cluster
# instead of at render time.
assert_explicit_tag = _image_of("vendor") == "ghcr.io/acme/vendor:3.0.0"

# Image with @sha256 digest and its own registry host — same passthrough.
assert_digest = _image_of("pinned-digest") == \
    "ghcr.io/acme/db@sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"

# The passthrough keys on the REGISTRY HOST, not on the tag: a tagged image
# with no host of its own is still an image THIS env builds/pushes, so it
# keeps taking the env registry. Without this, "has a tag" and "is
# third-party" collapse into one rule and every locally-built pinned image
# silently loses its registry.
assert_bare_tagged_keeps_registry = _image_of("baretag") == "localhost:5050/vendorapp:3.0.0"

# Pinned cronjob (one-shot Job) uses its pin.
assert_cronjob_pinned = _image_of("migrate") == "localhost:5050/reliant:v1.4.2"

# Pinned operator uses its pin.
assert_operator_pinned = _image_of("ctrl") == "localhost:5050/ctrl:v9.9.9"
