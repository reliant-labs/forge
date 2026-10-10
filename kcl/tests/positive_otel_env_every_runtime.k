"""
Positive test — every runtime a workload can run on gets the standard OTel
identity env, derived once in forge.render (the app <-> platform contract).

  * OTEL_SERVICE_NAME        = the workload's logical name
  * OTEL_RESOURCE_ATTRIBUTES = deployment.environment.name=<Bundle.env>,
                               service.namespace=<Bundle.project>,
                               service.version=<Bundle.release>

host / cluster / hosted carry it in `spec.env`; a compose service has no
spec env, so it carries it in the runtime's `env` (the compose PROCESS env).
A `tool` is never scheduled and `build-only` is never run: neither gets it.
"""

import forge
import forge.workloads as fw

_k3d = forge.ClusterTarget {cluster = "k3d-acme", namespace = "acme-dev"}
_pinned = "ghcr.io/acme/hosted-api@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

output = forge.render(forge.Bundle {
    project = "acme"
    env = "staging"
    release = "v1.2.3"
    control_plane = forge.ControlPlane {endpoint = "https://api.example.com"}
    workloads = [
        fw.Workload {name = "host-api", build = forge.GoBuild {cmd = "./cmd/acme"}, args = ["server"], runtime = forge.OnHost {}}
        fw.Workload {name = "compose-db", image = "postgres:16", runtime = forge.OnCompose {service = "db"}}
        fw.Workload {name = "cluster-worker", kind = "worker", image = "localhost:5050/acme", runtime = forge.OnCluster {target = _k3d}}
        fw.Workload {name = "hosted-api", image = _pinned, runtime = forge.OnHosted {}}
        fw.Workload {name = "a-tool", kind = "tool", build = forge.GoBuild {cmd = "./cmd/acme"}, args = ["x"], runtime = forge.OnHost {}}
        fw.Workload {name = "built-only", build = forge.GoBuild {cmd = "./cmd/acme"}, runtime = forge.BuildOnly {}}
    ]
})

_w = {w.name: w for w in output.workloads}
_spec_env = lambda name: str -> {str:str} {
    {e.name: e.value for e in (_w[name].spec.env or [])}
}

_ATTRS = "deployment.environment.name=staging,service.namespace=acme,service.version=v1.2.3"

assert_host_name = _spec_env("host-api").OTEL_SERVICE_NAME == "host-api"
assert_host_attrs = _spec_env("host-api").OTEL_RESOURCE_ATTRIBUTES == _ATTRS
assert_cluster_name = _spec_env("cluster-worker").OTEL_SERVICE_NAME == "cluster-worker"
assert_cluster_attrs = _spec_env("cluster-worker").OTEL_RESOURCE_ATTRIBUTES == _ATTRS
assert_hosted_name = _spec_env("hosted-api").OTEL_SERVICE_NAME == "hosted-api"
assert_hosted_attrs = _spec_env("hosted-api").OTEL_RESOURCE_ATTRIBUTES == _ATTRS

# Compose: the runtime's process env, keyed by the LOGICAL name, not the
# compose service name ("db").
_compose_env = _w["compose-db"].runtime.env
assert_compose_name = _compose_env.OTEL_SERVICE_NAME == "compose-db"
assert_compose_attrs = _compose_env.OTEL_RESOURCE_ATTRIBUTES == _ATTRS

# The cluster record (what kubectl applies) carries the same values.
_rec = [m for m in output.manifests if m.kind == "Workload" and m.metadata.name == "cluster-worker"][0]
assert_record_attrs = {e.name: e.value for e in _rec.spec.env}.OTEL_RESOURCE_ATTRIBUTES == _ATTRS

# The environment name is present on EVERY runtime that runs a process.
assert_env_name_everywhere = all n in ["host-api", "cluster-worker", "hosted-api"] {
    "deployment.environment.name=staging" in _spec_env(n).OTEL_RESOURCE_ATTRIBUTES
}

# A tool is never scheduled, a build-only workload never runs.
assert_tool_has_none = _spec_env("a-tool") == {}
assert_build_only_has_none = _spec_env("built-only") == {}
