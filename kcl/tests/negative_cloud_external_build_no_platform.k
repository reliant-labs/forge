"""
Negative test — a workload bound to a CLOUD cluster (a real remote registry)
that ships a REAL external image build MUST have a `platform` on its target.

This is the arm64-image-on-amd64-node footgun: without `platform`, the
external build pipeline falls through to the dev HOST's arch, so a `forge
build` from an arm64 Mac pushes arm64 images that exec-format-error on amd64
cloud nodes. The check is per workload now (the effective runtime decides
which target's platform applies), and it names `platform`.
"""

# expect: platform is required

import forge
import forge.workloads as fw

# Cloud target — a real remote registry, but NO platform stated.
_prod = forge.ClusterTarget {
    cluster = "gke_proj_us-central1_prod"
    namespace = "app-prod"
    registry = "ghcr.io/org/app"
}

output = forge.render(forge.Bundle {
    project = "app"
    workloads = [fw.Workload {
        name = "sibling"
        build = forge.DockerBuild {dockerfile = "docker/Dockerfile.sibling"}
        runtime = forge.OnCluster {target = _prod}
    }]
})
