"""
Negative test — a wildcard domain is refused.

A wildcard names an unbounded set of hostnames, and the platform verifies
ownership per name before serving it. There is nothing to verify.
"""

# expect: wildcard names an unbounded set

import forge

output = forge.render(forge.Bundle {
    project = "fixture"
    control_plane = forge.ControlPlane {}
    frontends = [forge.Frontend {
        name = "web"
        path = "web"
        domains = ["*.hounders.club"]
        runtime = forge.OnHosted {}
    }]
})
