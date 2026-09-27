"""
Positive test — a LOCAL env: control_plane + HostedSecrets, but NO hosted
tier. The control plane is only the env's secret store (values pulled into
memory by `forge env up`); every workload runs on the developer machine
(host / compose). The Bundle checks must admit this shape: none of the
hosted-coordinate refusals apply, because there is no hosted tier to
coordinate, and HostedSecrets' only requirement (control_plane) is met.
"""

import forge

_api = forge.RenderedWorkload {
    name = "api"
    deploy = forge.HostDeploy {
        runner = "go-run"
        env_vars = [forge.EnvVar {name = "STRIPE_SECRET_KEY", secret_ref = "app-secrets"}]
    }
}

_pg = forge.RenderedWorkload {
    name = "postgres"
    deploy = forge.Compose {service = "postgres"}
}

_bundle = forge.Bundle {
    project = "barksocial"
    env = "dev"
    control_plane = forge.ControlPlane {endpoint = "http://127.0.0.1:8090"}
    secret_provider = forge.HostedSecrets {}
    services = [_api, _pg]
}

output = forge.render(_bundle)
manifests = forge.render_manifests(_bundle)

assert_control_plane = output.control_plane.endpoint == "http://127.0.0.1:8090"
assert_hosted_provider = output.secret_provider.type == "hosted"
assert_host_service = output.services[0].deploy.type == "host"
assert_host_secret_ref = output.services[0].deploy.env_vars[0].secret_ref == "app-secrets"
assert_compose_service = output.services[1].deploy.type == "compose"
assert_no_hosted_tier = len([s for s in output.services if s.deploy.type == "simple-backend"]) == 0
assert_no_secret_manifest = len([m for m in manifests if m.kind == "Secret"]) == 0
