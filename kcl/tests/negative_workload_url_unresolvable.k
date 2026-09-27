"""
Negative test — on a non-hosted env, a reference to a workload with no URL
forge can know at render time is REFUSED rather than guessed. Here the host
service declares no listen_ports: `forge env up` hands it an ephemeral port
only after rendering, so any URL written now would be wrong.
"""

import forge

_bundle = forge.Bundle {
    services = [forge.RenderedWorkload { name = "api", deploy = forge.HostDeploy {} }]
    frontends = [forge.Frontend {
        name = "web"
        path = "frontends/web"
        runtime_config = { API_URL = forge.WorkloadURL { workload = "api" } }
    }]
}

output = forge.render(_bundle)
