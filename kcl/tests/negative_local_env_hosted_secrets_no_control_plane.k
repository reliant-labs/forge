"""
Negative test — the LOCAL shape without its control plane: host + compose
workloads with forge.HostedSecrets but no control_plane. A LOCAL env's values
live in (and are pulled from) its control plane, so without one there is
nowhere to read them from. Refused at load, not at `forge env up`.
"""

import forge

_bundle = forge.Bundle {
    services = [forge.RenderedWorkload {
        name = "api"
        deploy = forge.HostDeploy {runner = "go-run"}
    }]
    secret_provider = forge.HostedSecrets {}
}

output = forge.render(_bundle)
