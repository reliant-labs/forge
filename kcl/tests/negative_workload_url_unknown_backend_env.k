"""
Negative test — a SimpleBackend env var workloadURL naming an undeclared
workload is refused at load, on a HOSTED env too (where it would otherwise
reach the control plane and wait forever on a reference nothing satisfies).
"""

import forge
import forge.tiers

_bundle = forge.Bundle {
    control_plane = forge.ControlPlane { endpoint = "https://api.example.com" }
    services = [forge.RenderedWorkload {
        name = "api"
        deploy = forge.SimpleBackend {
            spec = tiers.SimpleBackend {
                image = "ghcr.io/acme/api:v1"
                ports = [8080]
                env = [tiers.EnvVar { name = "CORS_ORIGINS", workloadURL = forge.WorkloadURL { workload = "website" } }]
            }
        }
    }]
}

output = forge.render(_bundle)
