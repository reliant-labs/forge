"""
Positive test — FORGE_TRUSTED_EDGE marks the workloads Reliant's hosted proxy fronts.

  * forge.OnHosted   -> FORGE_TRUSTED_EDGE=reliant: forge/pkg/observe reads the
                        edge headers the proxy stamps (the visitor's daily hash,
                        the browser's privacy signal);
  * forge.OnCluster  -> nothing: a client could send those headers, so they are
                        never trusted off the platform;
  * an author's own FORGE_TRUSTED_EDGE wins.
"""

import forge
import forge.workloads as fw

_k3d = forge.ClusterTarget {cluster = "k3d-demo", namespace = "demo-dev"}

output = forge.render(forge.Bundle {
    project = "fixture"
    control_plane = forge.ControlPlane {}
    workloads = [
        fw.Workload {name = "api", image = "ghcr.io/acme/api:v1", ports = [fw.Port {name = "http", port = 8080, expose = True}], runtime = forge.OnHosted {}}
        fw.Workload {name = "own", image = "ghcr.io/acme/own:v1", env = {FORGE_TRUSTED_EDGE = "none"}, ports = [fw.Port {name = "http", port = 8081, expose = True}], runtime = forge.OnHosted {}}
        fw.Workload {name = "k8s", image = "ghcr.io/acme/k8s:v1", runtime = forge.OnCluster {target = _k3d}}
    ]
})

_env = lambda name: str -> {str:} {
    {e.name: e.value for w in output.workloads if w.name == name for e in w.spec.env if e.value}
}

assert_hosted_trusts_the_edge = _env("api").FORGE_TRUSTED_EDGE == "reliant"
assert_author_wins = _env("own").FORGE_TRUSTED_EDGE == "none"
assert_cluster_never = "FORGE_TRUSTED_EDGE" not in _env("k8s")
