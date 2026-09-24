"""
Negative test — a SimpleBackend on a SELF-HOSTED env (no control_plane) omits
its cluster/namespace. Self-hosted there is no platform to allocate them, so
both stay required.
"""

import forge
import forge.tiers

_bundle = forge.Bundle {
    services = [forge.RenderedWorkload {
        name = "api"
        deploy = forge.SimpleBackend {
            spec = tiers.SimpleBackend { image = "ghcr.io/acme/api:v1", ports = [8080] }
        }
    }]
}

output = forge.render(_bundle)
