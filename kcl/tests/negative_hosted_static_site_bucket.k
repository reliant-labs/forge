"""
Negative test — a StaticSite on a HOSTED env names a bucket.
The platform owns the bucket; a user naming one would be choosing where the
platform's credential writes. Refused.
"""

import forge

_bundle = forge.Bundle {
    control_plane = forge.ControlPlane { endpoint = "https://api.example.com" }
    services = []
    frontends = [forge.Frontend {
        name = "web"
        path = "frontends/web"
        type = "vite"
        deploy = forge.StaticSite { bucket = "gs://mine", public_dir = "dist" }
    }]
}

output = forge.render(_bundle)
