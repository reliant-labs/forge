"""
Negative test — a StaticSite with no bucket and no control_plane.

A StaticSite without a bucket is HOSTED (published into the platform's
bucket), which needs a control_plane to publish to. With neither there is
nowhere to publish. The rule is per frontend now, not env-wide.
"""

# expect: StaticSite.bucket is required

import forge

_bundle = forge.Bundle {
    frontends = [forge.Frontend {
        name = "web"
        path = "frontends/web"
        type = "vite"
        deploy = forge.StaticSite {public_dir = "dist"}
    }]
}

output = forge.render(_bundle)
