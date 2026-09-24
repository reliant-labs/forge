"""
Negative test — a HOSTED env also declares a cluster_target. Hosted tiers run
on the control plane's infrastructure and forge applies nothing locally, so an
env-wide cluster target is a contradiction. Refused.
"""

import forge

_bundle = forge.Bundle {
    control_plane = forge.ControlPlane { endpoint = "https://api.example.com" }
    cluster_target = forge.ClusterTarget { cluster = "c", namespace = "n", registry = "r.io/x" }
    services = []
}

output = forge.render(_bundle)
