"""
Positive test — a native application with associated_domains (https universal
links / App Links) and an rn frontend carrying its platform identities.

Domains alone do not change the rendered callbacks (forge derives the https
redirect where it reads app.json); a DECLARED redirect list in the author's
order stays untouched, and the frontend's signing identities render nowhere in
the identity block (they feed the association files at deploy).
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
            associated_domains = ["app.acme.example", "acme.example"]
            redirect_uris = ["https://app.acme.example/oauthredirect", "com.acme.mobile:/oauthredirect"]
        }]
    }
    workloads = [
        fw.Workload {name = "api", kind = "service", runtime = forge.OnHost {runner = "binary"}, build = forge.GoBuild {cmd = "./cmd/acme", output_name = "acme"}, args = ["server"]}
    ]
    frontends = [
        forge.Frontend {
            name = "mobile", path = "frontends/mobile", type = "rn", port = 8081, runtime = forge.OnHost {}
            apple_team_id = "23P64LQTZD"
            android_cert_fingerprints = {dev = ["debug"], prod = ["AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"]}
        }
    ]
})

_apps = {a.name: a for a in output.identity.applications}

assert_domains_render = _apps["dev:mobile"].associated_domains == ["app.acme.example", "acme.example"]
assert_redirect_order_is_kept = _apps["dev:mobile"].redirect_uris == ["https://app.acme.example/oauthredirect", "com.acme.mobile:/oauthredirect"]
assert_frontend_identities_render = output.frontends[0].apple_team_id == "23P64LQTZD"
assert_fingerprints_render = output.frontends[0].android_cert_fingerprints.dev == ["debug"]
