"""
Positive test — a native application with every callback declared on a HOSTED
identity (forge.ManagedIdentity, the default runtime): a private-use scheme and
an https (universal link) callback in the author's order. The control plane
registers it when it advertises the shape-identity-native-app feature; forge
withholds it, with a notice, from one that does not. Declared callbacks are
rendered untouched, and the frontend gets no second, derived application.
"""
# kcl-args: env="prod"

import forge
import forge.workloads as fw

_prod = forge.ClusterTarget {cluster = "gke_acme_prod", namespace = "acme-prod", connected_cluster = "acme-prod", platform = "amd64"}

output = forge.render(forge.Bundle {
    project = "acme"
    env = "prod"
    control_plane = forge.ControlPlane {}
    cluster_target = _prod
    identity = forge.ManagedIdentity {applications = [
        forge.IdentityApplication {name = "m", kind = "native", frontend = "mobile", redirect_uris = ["https://acme.example/oauthredirect", "com.acme.mobile:/oauthredirect"], logout_uris = ["https://acme.example/signedout", "com.acme.mobile:/signedout"]}
    ]}
    workloads = [fw.Workload {name = "api", image = "ghcr.io/acme/api", runtime = forge.OnCluster {target = _prod}}]
    frontends = [forge.Frontend {name = "mobile", path = "frontends/mobile", type = "rn", port = 8081, runtime = forge.OnHost {}}]
})

_apps = {a.name: a for a in output.identity.applications}

assert_runtime_is_hosted = output.identity.runtime == "hosted"
assert_declared_native_is_kept = _apps["m"].kind == "native" and _apps["m"].frontend == "mobile"
assert_redirect_order_is_kept = _apps["m"].redirect_uris == ["https://acme.example/oauthredirect", "com.acme.mobile:/oauthredirect"]
assert_logout_order_is_kept = _apps["m"].logout_uris == ["https://acme.example/signedout", "com.acme.mobile:/signedout"]
assert_no_second_derived_native = len([a for a in output.identity.applications if a.kind == "native"]) == 1
assert_native_has_no_broker = not _apps["m"].workload and not _apps["m"].login_uri
