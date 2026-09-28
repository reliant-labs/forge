"""
Negative test — a StaticSite that is HOSTED names a CDN or cache rules.

A StaticSite with no bucket publishes into the platform's bucket, behind the
platform's CDN: naming a URL map would be a user choosing what the platform's
credential purges, and cache_control has no carrier on the hosted release.
Refused rather than ignored.

(A bucket-bearing StaticSite in an env that ALSO declares control_plane is
legal now — hosting is per frontend, not env-wide — so the bucket itself is
what selects self-hosted.)
"""

# expect: StaticSite

import forge

_bundle = forge.Bundle {
    project = "fixture"
    control_plane = forge.ControlPlane {endpoint = "https://api.example.com"}
    frontends = [forge.Frontend {
        name = "web"
        path = "frontends/web"
        type = "vite"
        deploy = forge.StaticSite {
            public_dir = "dist"
            cdn = forge.StaticSiteCDN {url_map = "my-map"}
        }
    }]
}

output = forge.render(_bundle)
