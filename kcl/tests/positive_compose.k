"""
Positive test — Compose schema renders cleanly. Declarative-only;
the Go-side dispatch ships as a stub in this release.
"""

import forge

_svc = forge.Service {
    name = "worker"
    deploy = forge.Compose {
        compose_file = "deploy/docker-compose.prod.yml"
        service = "worker-prod"
        env_file = ".env.prod"
    }
}

_bundle = forge.Bundle { services = [_svc] }
output = forge.render(_bundle)

assert_discriminator = output.services[0].deploy.type == "compose"
assert_compose_file = output.services[0].deploy.compose_file == "deploy/docker-compose.prod.yml"
assert_service = output.services[0].deploy.service == "worker-prod"
assert_env_file = output.services[0].deploy.env_file == ".env.prod"

# Default compose_file when unset.
_default = forge.Service {
    name = "alt"
    deploy = forge.Compose {}
}
_b = forge.Bundle { services = [_default] }
_default_out = forge.render(_b)
assert_default_compose_file = _default_out.services[0].deploy.compose_file == "docker-compose.yml"
