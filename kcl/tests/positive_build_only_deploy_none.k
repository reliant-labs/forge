"""
Positive test — `deploy = None` is the explicit BUILD-ONLY mode.

A forge.RenderedWorkload with a `build` and an explicit `deploy = None` (or an
omitted deploy) builds + pushes its image but renders NO Deployment /
Service / RBAC / owned manifests. Use it for a base image other
workloads reference at runtime.

The render layer projects an explicit `{type: "build-only"}` deploy
block (mirroring forge.BuildOnly) so the Go consumer dispatches it the
same way: in the build set, out of the deploy set. render_manifests
emits NOTHING for it (it isn't `type == "cluster"`).

Exercises:
  * deploy = None projects deploy.type == "build-only".
  * An omitted deploy projects the same (the two are equivalent).
  * The service's `build` (a DockerBuild here — the image build) projects
    through, so the build phase still builds + pushes the image.
  * A build-only service renders NO Deployment/Service in the manifest
    stream, even though a normal cluster service in the SAME bundle does.
"""

import forge

# A base image: built + pushed, never orchestrated. deploy = None is
# explicit build-only.
_base_image = forge.RenderedWorkload {
    name = "workspace-base"
    image = "workspace-base"
    deploy = None
    build = forge.DockerBuild {
        dockerfile = "docker/Dockerfile.workspace-base"
    }
}

# The omitted-deploy equivalent — same projection as deploy = None.
_cli = forge.RenderedWorkload {
    name = "reliant-cli"
    build = forge.GoBuild { cmd = "./cmd/reliant" }
}

# A normal cluster service in the SAME bundle, to prove the build-only
# ones don't suppress real workload manifests.
_target = forge.ClusterTarget {
    cluster = "k3d-cp"
    namespace = "dev"
    registry = "localhost:5051"
}

_api = forge.RenderedWorkload {
    name = "api"
    image = "api"
    deploy = _target.deploy | {ports = [8080]}
}

_bundle = forge.Bundle {
    cluster_target = _target
    services = [_base_image, _cli, _api]
}

output = forge.render(_bundle)
manifests = forge.render_manifests(_bundle, "latest", {}, False)

# ── Invariants ─────────────────────────────────────────────────────

# deploy = None projects an explicit build-only block.
assert_base_deploy_type = output.services[0].deploy.type == "build-only"
# The image build projects through so the build phase builds + pushes it.
assert_base_build_type = output.services[0].build.type == "docker"
assert_base_dockerfile = output.services[0].build.dockerfile == "docker/Dockerfile.workspace-base"

# An omitted deploy is equivalent to deploy = None.
assert_cli_deploy_type = output.services[1].deploy.type == "build-only"
assert_cli_build_type = output.services[1].build.type == "go"

# The normal cluster service is unaffected.
assert_api_deploy_type = output.services[2].deploy.type == "cluster"

# The build-only services render NO Deployment / Service. The normal `api`
# service DOES — so the manifest stream carries exactly api's workload.
assert_no_base_deploy = len([m for m in manifests \
    if m.kind == "Deployment" and m.metadata.name == "workspace-base"]) == 0
assert_no_cli_deploy = len([m for m in manifests \
    if m.kind == "Deployment" and m.metadata.name == "reliant-cli"]) == 0
assert_api_deploy_present = len([m for m in manifests \
    if m.kind == "Deployment" and m.metadata.name == "api"]) == 1
