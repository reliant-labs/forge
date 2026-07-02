"""
Negative test — HostDeploy.air_config is only valid when runner =
"air". Setting it with runner="go-run" must fail the check block.
"""

import forge

_svc = forge.K8sWorkload {
    name = "bad-air"
    image = "x"
    deploy = forge.HostDeploy {
        runner = "go-run"
        air_config = ".air.toml"
    }
}
