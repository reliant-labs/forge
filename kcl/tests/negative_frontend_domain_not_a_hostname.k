"""
Negative test — a domain that is not a lowercase DNS hostname is refused at
load, not at the control plane.

DNS is case-insensitive but the platform stores and compares the name it was
given, so an uppercase declaration is a claim that never matches.
"""

# expect: must be lowercase DNS hostnames

import forge

output = forge.render(forge.Bundle {
    project = "fixture"
    control_plane = forge.ControlPlane {}
    frontends = [forge.Frontend {
        name = "web"
        path = "web"
        domains = ["https://Hounders.club"]
        runtime = forge.OnHosted {}
    }]
})
