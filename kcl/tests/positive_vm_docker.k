"""
Positive test — VMDocker schema renders cleanly. Declarative-only;
the Go-side dispatch ships as a stub in this release.
"""

import forge

_svc = forge.Service {
    name = "edge-proxy"
    deploy = forge.VMDocker {
        ssh_host = "ubuntu@vultr-prod"
        image = "kalshi/edge"
        tag = "v1.0.0"
        # Note: $${IMAGE}/$${TAG} (literal dollar dollar) — KCL escapes
        # `$` in string literals; the forge CLI will substitute IMAGE/TAG
        # before invoking the shell.
        deploy_cmd = "docker pull image:tag && docker run -d image:tag"
    }
}

_bundle = forge.Bundle { services = [_svc] }
output = forge.render(_bundle)

assert_discriminator = output.services[0].deploy.type == "vm-docker"
assert_ssh_host = output.services[0].deploy.ssh_host == "ubuntu@vultr-prod"
assert_image = output.services[0].deploy.image == "kalshi/edge"
assert_tag = output.services[0].deploy.tag == "v1.0.0"
assert_deploy_cmd_set = output.services[0].deploy.deploy_cmd != None
assert_health_cmd_null = output.services[0].deploy.health_cmd == None
