"""
Positive test — the build-platform guard's three PASSING shapes, the
counterpart to negative_cloud_external_build_no_platform.k:

  1. Cloud target + real external build + `platform` SET → renders.
  2. LOCAL target (localhost registry) + real external build + NO platform →
     renders (local k3d nodes share the host arch; an omitted platform
     correctly tracks the host — the dev/e2e path).
  3. Cloud target + NO platform but only a NO-OP ShellBuild (`cmd = "true"`,
     the prebuilt-image-pull hatch that pushes nothing) → renders (a no-op
     build produces no image, so it imposes no arch requirement).

Also pins the forge.builds_an_image / forge.is_local_registry predicates the
guard keys on.
"""

import forge

# ── shape 1: cloud + real build + platform set ─────────────────────────────
_cloud = forge.ClusterTarget {
    cluster = "gke_proj_us-central1_prod"
    namespace = "app-prod"
    registry = "ghcr.io/org/app"
    platform = "amd64"
}
_real_build_svc = forge.K8sWorkload {
    name = "sibling"
    image = "sibling"
    build = forge.ShellBuild {
        cmd = r"docker build --platform=linux/${TARGETARCH} -t ${REGISTRY}/${IMAGE}:${TAG} . && docker push ${REGISTRY}/${IMAGE}:${TAG}"
    }
    deploy = _cloud.deploy
}
_cloud_out = forge.render(forge.Bundle {
    cluster_target = _cloud
    services = [_real_build_svc]
})
assert_cloud_renders = _cloud_out.cluster_target.platform == "amd64"

# ── shape 2: local target + real build + NO platform (exempt) ──────────────
_local = forge.ClusterTarget {
    cluster = "k3d-dev"
    namespace = "dev"
    registry = "localhost:5051"
}
_local_out = forge.render(forge.Bundle {
    cluster_target = _local
    services = [_real_build_svc | {deploy = _local.deploy}]
})
assert_local_renders = _local_out.cluster_target.platform == None

# ── shape 3: cloud + no platform but only a NO-OP ShellBuild (exempt) ──────
_noop_svc = forge.K8sWorkload {
    name = "prebuilt"
    image = "prebuilt"
    build = forge.ShellBuild { cmd = "true" }
    deploy = _cloud.deploy
}
_cloud_noplatform = forge.ClusterTarget {
    cluster = "gke_proj_us-central1_prod"
    namespace = "app-prod"
    registry = "ghcr.io/org/app"
}
_noop_out = forge.render(forge.Bundle {
    cluster_target = _cloud_noplatform
    services = [_noop_svc | {deploy = _cloud_noplatform.deploy}]
})
assert_noop_renders = _noop_out.cluster_target.platform == None

# ── predicate pins ─────────────────────────────────────────────────────────
assert_is_local_localhost = forge.is_local_registry("localhost:5051")
assert_is_local_registry_localhost = forge.is_local_registry("registry.localhost:5051")
assert_is_cloud_ghcr = not forge.is_local_registry("ghcr.io/org/app")
assert_builds_real_shell = forge.builds_an_image(_real_build_svc)
assert_noop_not_build = not forge.builds_an_image(_noop_svc)
