"""
Positive test — `native_sign_in = False` on an `rn` frontend of a host-run
managed identity derives NO native client for it, while a second `rn`
frontend that leaves the default still gets one. The broker application for
the `api` workload is unaffected.
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
        forge.Frontend {name = "kiosk", path = "frontends/kiosk", type = "rn", port = 8082, native_sign_in = False, runtime = forge.OnHost {}}
        forge.Frontend {name = "mobile", path = "frontends/mobile", type = "rn", port = 8081, runtime = forge.OnHost {}}
    ]
})

_apps = {a.name: a for a in output.identity.applications}

assert_opted_out_frontend_has_no_client = "dev:kiosk" not in _apps
assert_default_frontend_still_has_one = _apps["dev:mobile"].kind == "native" and _apps["dev:mobile"].frontend == "mobile"
assert_broker_still_derived = _apps["dev:api"].kind == "native_broker"
assert_exactly_two_applications = len(_apps) == 2
