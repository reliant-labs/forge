"""
Positive test — a mobile (`rn`) frontend in an env whose identity runs on the
host IdP gets its native OIDC client DERIVED, with no declaration: a public
client of the frontend, no broker workload, no login_uri. The callbacks are
left empty here because `forge env up` reads them from the app's app.json.
The broker application for the `api` workload is still derived as before.
"""
# kcl-args: env="dev"

import forge
import forge.workloads as fw

output = forge.render(forge.Bundle {
    project = "acme"
    env = "dev"
    lifecycle = "local"
    infra = [
        forge.HostInfra {name = "pg", engine = "postgres", port = 5432}
        forge.HostInfra {name = "idp", engine = "zitadel", port = 8098, idp_database_port = 5432}
    ]
    identity = forge.ManagedIdentity {
        runtime = forge.IdentityOnHost {infra = "idp"}
        roles = [forge.IdentityRole {key = "admin"}]
    }
    workloads = [
        fw.Workload {name = "api", kind = "service", runtime = forge.OnHost {runner = "binary"}, build = forge.GoBuild {cmd = "./cmd/acme", output_name = "acme"}, args = ["server"]}
    ]
    frontends = [
        forge.Frontend {name = "mobile", path = "frontends/mobile", type = "rn", port = 8081, runtime = forge.OnHost {}}
        forge.Frontend {name = "web", path = "frontends/web", type = "vite", port = 5173, runtime = forge.OnHost {}}
    ]
})

_apps = {a.name: a for a in output.identity.applications}

assert_two_applications = len(_apps) == 2
assert_broker_still_derived = _apps["dev:api"].kind == "native_broker" and _apps["dev:api"].workload == "api"
assert_native_derived_for_the_rn_frontend = _apps["dev:mobile"].kind == "native" and _apps["dev:mobile"].frontend == "mobile"
assert_native_has_no_broker = not _apps["dev:mobile"].workload and not _apps["dev:mobile"].login_uri
assert_native_callbacks_come_from_the_app = not _apps["dev:mobile"].redirect_uris
assert_no_application_for_the_web_frontend = "dev:web" not in _apps
assert_consumer_is_still_the_service = [c.workload for c in output.identity.consumers] == ["api"]
