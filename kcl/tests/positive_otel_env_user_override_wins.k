"""
Positive test — a value the author set is never clobbered.

Forge derives OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES only when the
workload's own env does not name them. "Names them" is presence, not
truthiness: an author's empty string is a deliberate value and is kept.
OTEL_RESOURCE_ATTRIBUTES is all-or-nothing — forge does not splice its keys
into a string the author wrote.

For a compose workload the runtime's own `env` counts as the author's too.
"""

import forge
import forge.workloads as fw

_k3d = forge.ClusterTarget {cluster = "k3d-acme", namespace = "acme-dev"}

output = forge.render(forge.Bundle {
    project = "acme"
    env = "dev"
    release = "v9"
    workloads = [
        fw.Workload {
            name = "both-set"
            build = forge.GoBuild {cmd = "./cmd/acme"}
            args = ["server"]
            env = {OTEL_SERVICE_NAME = "custom-svc", OTEL_RESOURCE_ATTRIBUTES = "team=payments"}
            runtime = forge.OnHost {}
        }
        fw.Workload {
            name = "name-only"
            image = "localhost:5050/acme"
            env = {OTEL_SERVICE_NAME = "custom-svc"}
            runtime = forge.OnCluster {target = _k3d}
        }
        fw.Workload {
            name = "empty-wins"
            build = forge.GoBuild {cmd = "./cmd/acme"}
            args = ["server"]
            env = {OTEL_SERVICE_NAME = "", OTEL_RESOURCE_ATTRIBUTES = ""}
            runtime = forge.OnHost {}
        }
        fw.Workload {
            name = "compose-override"
            image = "postgres:16"
            runtime = forge.OnCompose {service = "db", env = {OTEL_SERVICE_NAME = "pg"}}
        }
    ]
})

_w = {w.name: w for w in output.workloads}
_env = lambda name: str -> {str:str} {
    {e.name: e.value for e in _w[name].spec.env}
}

assert_both_kept = _env("both-set").OTEL_SERVICE_NAME == "custom-svc" and _env("both-set").OTEL_RESOURCE_ATTRIBUTES == "team=payments"
assert_no_duplicates = len([e for e in _w["both-set"].spec.env]) == 2
# Only the missing one is derived.
assert_name_kept_attrs_derived = _env("name-only").OTEL_SERVICE_NAME == "custom-svc" and _env("name-only").OTEL_RESOURCE_ATTRIBUTES == "deployment.environment.name=dev,service.namespace=acme,service.version=v9"
assert_empty_is_a_value = _env("empty-wins").OTEL_SERVICE_NAME == "" and _env("empty-wins").OTEL_RESOURCE_ATTRIBUTES == ""
# The author's own order is untouched; derived entries come after.
assert_author_order = [e.name for e in _w["both-set"].spec.env] == ["OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES"]
assert_derived_last = [e.name for e in _w["name-only"].spec.env] == ["OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES"]
# Compose: the runtime env wins over derivation; the missing key is derived.
_c = _w["compose-override"].runtime.env
assert_compose_runtime_env_wins = _c.OTEL_SERVICE_NAME == "pg" and _c.OTEL_RESOURCE_ATTRIBUTES.startswith("deployment.environment.name=dev")
