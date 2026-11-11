"""
Positive test — an `rn` frontend of an env on HOSTED managed identity gets its
native OIDC client derived, as it does on the host-run IdP: a public client of
the frontend, no broker workload, no login_uri. The callbacks are left empty
here because KCL cannot read app.json; `forge env shape`/`build`/`deploy`
resolve them from `expo.scheme` and send the control plane the resolved
declaration. The broker application for the `api` workload is still derived,
and the web frontend gets none.
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
        forge.Frontend {name = "mobile", path = "frontends/mobile", type = "rn", port = 8081, runtime = forge.OnHost {}}
        forge.Frontend {name = "web", path = "frontends/web", type = "vite", port = 5173, runtime = forge.OnHost {}}
    ]
})

_apps = {a.name: a for a in output.identity.applications}

assert_runtime_is_hosted = output.identity.runtime == "hosted"
assert_two_applications = len(_apps) == 2
assert_broker_still_derived = _apps["prod:api"].kind == "native_broker" and _apps["prod:api"].workload == "api"
assert_native_derived_for_the_rn_frontend = _apps["prod:mobile"].kind == "native" and _apps["prod:mobile"].frontend == "mobile"
assert_native_has_no_broker = not _apps["prod:mobile"].workload and not _apps["prod:mobile"].login_uri
assert_native_callbacks_come_from_the_app = not _apps["prod:mobile"].redirect_uris and not _apps["prod:mobile"].logout_uris
assert_no_application_for_the_web_frontend = "prod:web" not in _apps
assert_consumer_is_still_the_service = [c.workload for c in output.identity.consumers] == ["api"]
