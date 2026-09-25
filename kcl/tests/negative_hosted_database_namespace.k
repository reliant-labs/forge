"""
Negative test — a ManagedDatabase on a HOSTED env names a namespace.
The control plane allocates it. Refused.
"""

import forge

_bundle = forge.Bundle {
    control_plane = forge.ControlPlane { endpoint = "https://api.example.com" }
    services = []
    databases = [forge.ManagedDatabase { name = "orders", namespace = "acme-prod" }]
}

output = forge.render(_bundle)
