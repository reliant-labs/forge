"""
Negative test — a StaticSite on a SELF-HOSTED env names no bucket.
With no platform to assign one, there is nowhere to publish. Refused.
"""

import forge

_bundle = forge.Bundle {
    services = []
    frontends = [forge.Frontend {
        name = "web"
        path = "frontends/web"
        type = "vite"
        deploy = forge.StaticSite { public_dir = "dist" }
    }]
}

output = forge.render(_bundle)
