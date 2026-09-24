"""
Positive test — a HOSTED env declares a StaticSite with NO bucket.

The platform owns the bucket, the prefix and the CDN; forge ships the
assembled site as an OCI release artifact. The entity still carries the build
inputs (public_dir, base_path, keep_releases) the hosted provider needs.
"""

import forge

_bundle = forge.Bundle {
    project = "acme"
    env = "hosted"
    control_plane = forge.ControlPlane { endpoint = "https://api.example.com" }
    services = []
    frontends = [forge.Frontend {
        name = "web"
        path = "frontends/web"
        type = "vite"
        deploy = forge.StaticSite { public_dir = "dist", base_path = "/app", keep_releases = 5 }
    }]
}

_out = forge.render(_bundle)
_d = _out.frontends[0].deploy
assert_type = _d.type == "static-site"
assert_no_bucket = _d.bucket == ""
assert_public_dir = _d.public_dir == "dist"
assert_keep = _d.keep_releases == 5
