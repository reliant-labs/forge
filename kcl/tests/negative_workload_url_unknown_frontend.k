"""
Negative test — a frontend runtime_config forge.WorkloadURL naming a workload
the environment does not declare is refused at LOAD time, with the declared
names listed. A dangling reference would otherwise ship a config.js whose
API_URL is empty, and the app would call its own origin.
"""

import forge

_bundle = forge.Bundle {
    services = [forge.RenderedWorkload { name = "api", deploy = forge.HostDeploy { listen_ports = [8085] } }]
    frontends = [forge.Frontend {
        name = "web"
        path = "frontends/web"
        runtime_config = { API_URL = forge.WorkloadURL { workload = "apiserver" } }
    }]
}

output = forge.render(_bundle)
