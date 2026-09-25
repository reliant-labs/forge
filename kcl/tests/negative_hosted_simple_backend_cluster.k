"""
Negative test — a SimpleBackend on a HOSTED env names a cluster/namespace.

The control plane allocates both; a coordinate the author wrote would be a
second, unverified opinion about where the workload lands. Refused.
"""

import forge
import forge.tiers

_bundle = forge.Bundle {
    control_plane = forge.ControlPlane { endpoint = "https://api.example.com" }
    services = [forge.RenderedWorkload {
        name = "api"
        deploy = forge.SimpleBackend {
            cluster = "gke_acme_prod"
            namespace = "acme-prod"
            spec = tiers.SimpleBackend { image = "ghcr.io/acme/api:v1", ports = [8080] }
        }
    }]
}

output = forge.render(_bundle)
