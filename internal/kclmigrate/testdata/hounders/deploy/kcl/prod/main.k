"""hounders prod — a HOSTED env: forge.ControlPlane carried the registry.

Every workload binds forge.OnHosted, and none declares an image. The registry
lives on the ControlPlane declaration, as a literal.
"""

import forge
import forge.workloads as fw
import ..workloads as wl

control_plane = forge.ControlPlane {
    registry = "ghcr.io/reliant-01"
}

_hosted = forge.OnHosted {}

web = forge.Frontend {
    name = "web"
    path = "frontends/web"
    runtime = _hosted
}

_bundle = forge.Bundle {
    project = "hounders"
    control_plane = control_plane
    frontends = [web]
    workloads = [
        wl.migrate | {runtime = _hosted}
        wl.membership | {runtime = _hosted}
    ]
}

output = forge.render(_bundle)
