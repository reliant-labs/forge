"""
Positive test — the build-platform guard passes in the three shapes that
must NOT be refused (the refusal is negative_cloud_external_build_no_platform.k):

  1. a CLOUD target that states `platform`;
  2. a LOCAL registry target (k3d nodes share the dev host's arch, so an
     omitted platform correctly tracks the host);
  3. a no-op ShellBuild (`true`) on a cloud target without a platform —
     nothing is built, so there is no arch to get wrong.

It also pins the predicates the rule is built on: forge.is_local_registry
(the registry is the authoritative local-vs-cloud signal) and
forge.builds_an_image (retyped to a Workload: a DockerBuild or a non-no-op
ShellBuild; a RemoteBuild states its own platform).
"""

import forge
import forge.workloads as fw

_cloud = forge.ClusterTarget {cluster = "gke_proj_us-central1_prod", namespace = "app-prod", registry = "ghcr.io/org/app", platform = "amd64"}
_local = forge.ClusterTarget {cluster = "k3d-dev", namespace = "dev", registry = "localhost:5051"}
_cloud_noplatform = forge.ClusterTarget {cluster = "gke_proj_us-central1_prod", namespace = "app-prod", registry = "ghcr.io/org/app"}

_real = fw.Workload {
    name = "sibling"
    image = "sibling"
    build = forge.ShellBuild {cmd = "docker build --platform=linux/${forge.target_arch()} -t sibling . && docker push sibling"}
}
_docker = fw.Workload {name = "gw", image = "gw", build = forge.DockerBuild {dockerfile = "Dockerfile.gw"}}
_noop = fw.Workload {name = "prebuilt", image = "prebuilt", build = forge.ShellBuild {cmd = "true"}}

_cloud_out = forge.render(forge.Bundle {project = "app", workloads = [_real | {runtime = forge.OnCluster {target = _cloud}}]})
assert_cloud_renders = [w for w in _cloud_out.workloads][0].runtime.platform == "amd64"

_local_out = forge.render(forge.Bundle {project = "app", workloads = [_real | {runtime = forge.OnCluster {target = _local}}]})
assert_local_renders = _local_out.workloads[0].runtime.platform == None

_noop_out = forge.render(forge.Bundle {project = "app", workloads = [_noop | {runtime = forge.OnCluster {target = _cloud_noplatform}}]})
assert_noop_renders = _noop_out.workloads[0].runtime.platform == None

assert_is_local_localhost = forge.is_local_registry("localhost:5051")
assert_is_local_registry_localhost = forge.is_local_registry("registry.localhost:5051")
assert_is_cloud_ghcr = not forge.is_local_registry("ghcr.io/org/app")
assert_builds_real_shell = forge.builds_an_image(_real)
assert_builds_docker = forge.builds_an_image(_docker)
assert_noop_not_build = not forge.builds_an_image(_noop)
