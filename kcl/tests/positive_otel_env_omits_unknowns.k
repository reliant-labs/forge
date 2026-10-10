"""
Positive test — forge reports only what it knows.

With no Bundle.release the `service.version` key is OMITTED (never `latest`,
never empty), and with no Bundle.env (a hand `kcl run` without `-D env=`) the
`deployment.environment.name` key is omitted too. `service.namespace` is the
project, which is always known.
"""

import forge
import forge.workloads as fw

_attrs = lambda o: any -> str {
    {e.name: e.value for e in o.workloads[0].spec.env}.OTEL_RESOURCE_ATTRIBUTES
}

_wl = fw.Workload {name = "api", build = forge.GoBuild {cmd = "./cmd/acme"}, args = ["server"], runtime = forge.OnHost {}}

_no_release = forge.render(forge.Bundle {project = "acme", env = "dev", workloads = [_wl]})
_no_env = forge.render(forge.Bundle {project = "acme", env = "", release = "v2", workloads = [_wl]})
_neither = forge.render(forge.Bundle {project = "acme", env = "", workloads = [_wl]})

assert_no_release_omits_version = _attrs(_no_release) == "deployment.environment.name=dev,service.namespace=acme"
assert_no_env_omits_environment = _attrs(_no_env) == "service.namespace=acme,service.version=v2"
assert_neither = _attrs(_neither) == "service.namespace=acme"

# Values that carry attribute separators cannot split an attribute.
_odd = forge.render(forge.Bundle {project = "a,b=c", env = "dev", workloads = [_wl]})
assert_values_are_encoded = _attrs(_odd) == "deployment.environment.name=dev,service.namespace=a%2Cb%3Dc"
