"""
Positive test — a native application declared by hand on the host-run
identity: a private-use reverse-DNS callback and an https (universal link)
callback side by side, in the author's order (the first is the one the app
signs in with). It replaces nothing: the `api` broker application is still
derived, and the frontend gets no second, derived native application.

(Hosted identity refuses kind native until hosted registration lands; see
negative_identity_native_hosted_declared.k.)
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
        applications = [forge.IdentityApplication {
            name = "dev:mobile", kind = "native", frontend = "mobile"
            redirect_uris = ["com.acme.mobile:/oauthredirect", "https://acme.example/oauthredirect"]
            logout_uris = ["com.acme.mobile:/signedout", "https://acme.example/signedout"]
        }]
    }
    workloads = [
        fw.Workload {name = "api", kind = "service", runtime = forge.OnHost {runner = "binary"}, build = forge.GoBuild {cmd = "./cmd/acme", output_name = "acme"}, args = ["server"]}
    ]
    frontends = [
        forge.Frontend {name = "mobile", path = "frontends/mobile", type = "rn", port = 8081, runtime = forge.OnHost {}}
    ]
})

_apps = {a.name: a for a in output.identity.applications}

assert_native_and_broker = sorted(_apps) == ["dev:api", "dev:mobile"]
assert_redirect_order_is_kept = _apps["dev:mobile"].redirect_uris == ["com.acme.mobile:/oauthredirect", "https://acme.example/oauthredirect"]
assert_logout_order_is_kept = _apps["dev:mobile"].logout_uris == ["com.acme.mobile:/signedout", "https://acme.example/signedout"]
assert_no_native_consumer = [c.workload for c in output.identity.consumers] == ["api"]
