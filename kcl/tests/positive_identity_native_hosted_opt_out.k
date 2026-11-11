"""
Positive test — `native_sign_in = False` on an `rn` frontend of a HOSTED
managed identity derives NO native client for it, while a second `rn` frontend
that leaves the default still gets one. The opt-out is the same on every
identity runtime.
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
    identity = forge.ManagedIdentity {roles = [forge.IdentityRole {key = "admin"}]}
    workloads = [fw.Workload {name = "api", image = "ghcr.io/acme/api", runtime = forge.OnCluster {target = _prod}}]
    frontends = [
        forge.Frontend {name = "kiosk", path = "frontends/kiosk", type = "rn", port = 8082, native_sign_in = False, runtime = forge.OnHost {}}
        forge.Frontend {name = "mobile", path = "frontends/mobile", type = "rn", port = 8081, runtime = forge.OnHost {}}
    ]
})

_apps = {a.name: a for a in output.identity.applications}

assert_opted_out_frontend_has_no_client = "prod:kiosk" not in _apps
assert_default_frontend_still_has_one = _apps["prod:mobile"].kind == "native" and _apps["prod:mobile"].frontend == "mobile"
assert_broker_still_derived = _apps["prod:api"].kind == "native_broker"
assert_exactly_two_applications = len(_apps) == 2
