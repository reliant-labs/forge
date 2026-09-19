"""
Negative test — StaticSiteCDN.invalidate rejects a value outside the
enum.

The enum is closed on purpose. Invalidation is rate-limited per project
and each request is a global purge, so the set of policies forge is
willing to execute is a deliberate, reviewed list — not a passthrough
for whatever string a main.k happens to carry. A typo'd "entrypoint"
must fail at render, not fall through to some default and quietly stop
invalidating a production site's HTML.
"""

import forge

_fe = forge.Frontend {
    name = "web"
    path = "web"
    deploy = forge.StaticSite {
        bucket = "gs://b"
        public_dir = "dist"
        cdn = forge.StaticSiteCDN {
            url_map = "lb"
            invalidate = "entrypoint"
        }
    }
}
