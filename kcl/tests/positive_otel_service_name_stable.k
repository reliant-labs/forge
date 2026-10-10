"""
Positive test — a service name is IDENTITY, so it survives a runtime move.

One declaration, bound to two runtimes in one bundle. The second binding must
take a different `name` (two workloads of a bundle cannot share one), which
is exactly the case that used to change the service name: deriving it from
the binding's rendered name made `admin-server` and `admin-server-k8s` two
services in the telemetry backend.

`service_name` on the DECLARATION is the stable key; both bindings inherit it.
Without it the declared `name` is used, so a project that never renames a
binding needs to write nothing.
"""

import forge
import forge.workloads as fw

_k3d = forge.ClusterTarget {cluster = "k3d-cp", namespace = "cp-dev"}

_admin = fw.Workload {
    name = "admin-server"
    service_name = "admin-server"
    build = forge.GoBuild {cmd = "./cmd/control-plane"}
    image = "localhost:5051/control-plane"
    args = ["admin"]
}

_host = _admin | {runtime = forge.OnHost {}}
_cluster = _admin | {name = "admin-server-k8s", runtime = forge.OnCluster {target = _k3d}}
# A compose service runs a prebuilt image: no build, no args.
_compose = fw.Workload {name = "admin-server-compose", service_name = "admin-server", image = "ghcr.io/acme/admin:v1", runtime = forge.OnCompose {service = "admin"}}

output = forge.render(forge.Bundle {project = "cp", env = "dev", workloads = [_host, _cluster, _compose]})
_w = {w.name: w for w in output.workloads}
_svc = lambda name: str -> str {
    {e.name: e.value for e in _w[name].spec.env}.OTEL_SERVICE_NAME
}

assert_host_cluster_same = _svc("admin-server") == _svc("admin-server-k8s")
assert_stable_value = _svc("admin-server") == "admin-server"
assert_compose_same = _w["admin-server-compose"].runtime.env.OTEL_SERVICE_NAME == "admin-server"
# The whole telemetry identity, not just the name, is runtime-independent.
_attrs = lambda name: str -> str {
    {e.name: e.value for e in _w[name].spec.env}.OTEL_RESOURCE_ATTRIBUTES
}
assert_attrs_same = _attrs("admin-server") == _attrs("admin-server-k8s")

# No service_name: the declared name is the identity.
_plain = forge.render(forge.Bundle {project = "cp", env = "dev", workloads = [fw.Workload {name = "worker", kind = "worker", build = forge.GoBuild {cmd = "./cmd/x"}, args = ["worker"], runtime = forge.OnHost {}}]})
assert_default_is_declared_name = {e.name: e.value for e in _plain.workloads[0].spec.env}.OTEL_SERVICE_NAME == "worker"
