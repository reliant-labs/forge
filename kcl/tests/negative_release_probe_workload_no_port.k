"""
Negative test — a release probe declared on a hosted workload that exposes no
port. The platform has no hostname for it, so it could never be probed:
refused, naming the workload, rather than silently ignored.
"""

# expect: Workload 'worker': release_probe is declared, but the workload exposes no port

import forge
import forge.workloads as fw

output = forge.render(forge.Bundle {
    project = "fixture"
    control_plane = forge.ControlPlane {}
    workloads = [fw.Workload {
        name = "worker"
        image = "ghcr.io/acme/worker:v1"
        ports = [fw.Port {name = "http", port = 9090}]
        release_probe = forge.Probe {paths = ["/ok"]}
        runtime = forge.OnHosted {}
    }]
})
