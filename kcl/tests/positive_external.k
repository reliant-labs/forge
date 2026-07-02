"""
Positive test — External schema renders cleanly. Generic shell-command
deploy target consumed by the ExternalProvider in
internal/deploytarget/external.go.
"""

import forge

# A flyctl-style external deploy. Raw-string literals (r"...") keep
# KCL from interpreting ${X} as KCL interpolation — the tokens land in
# the rendered JSON verbatim so the Go-side runner can expand them.
_svc = forge.K8sWorkload {
    name = "edge-proxy"
    image = "ghcr.io/x/edge"
    deploy = forge.External {
        deploy_cmd = r"flyctl deploy --image ${IMAGE}:${TAG} --app edge-prod"
        rollback_cmd = r"flyctl deploy --image ${IMAGE}:${LAST_TAG} --app edge-prod"
        health_cmd = "flyctl status --app edge-prod"
        env_file = ".env.prod"
        env = {"FLY_APP" = "edge-prod"}
    }
}

_bundle = forge.Bundle { services = [_svc] }
output = forge.render(_bundle)

assert_discriminator = output.services[0].deploy.type == "external"
assert_deploy_cmd_set = output.services[0].deploy.deploy_cmd != None
assert_rollback_cmd_set = output.services[0].deploy.rollback_cmd != None
assert_health_cmd_set = output.services[0].deploy.health_cmd != None
assert_env_file = output.services[0].deploy.env_file == ".env.prod"
assert_env_map = output.services[0].deploy.env["FLY_APP"] == "edge-prod"

# Minimal shape — only deploy_cmd is required.
_min = forge.K8sWorkload {
    name = "minimal"
    deploy = forge.External { deploy_cmd = "echo deploy" }
}
_b = forge.Bundle { services = [_min] }
_min_out = forge.render(_b)
assert_min_discriminator = _min_out.services[0].deploy.type == "external"
assert_min_rollback_null = _min_out.services[0].deploy.rollback_cmd == None
assert_min_health_null = _min_out.services[0].deploy.health_cmd == None
assert_min_env_file_null = _min_out.services[0].deploy.env_file == None
assert_min_env_empty = len(_min_out.services[0].deploy.env) == 0
