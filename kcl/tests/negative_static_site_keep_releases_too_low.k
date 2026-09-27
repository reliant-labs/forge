"""
Negative test — StaticSite.keep_releases = 1 is rejected.

Retaining a single release deletes the previous release's archive while
pages loaded from it moments ago are still fetching its hashed assets. The schema refuses the value rather than
silently clamping it to 2: a policy the author wrote and forge quietly
ignored is worse than an error, because it reads as accepted.

0 ("retain everything") and >= 2 are the legal values —
positive_frontend_static_site_defaults.k pins that 0 survives.
"""

import forge

_fe = forge.Frontend {
    name = "web"
    path = "web"
    deploy = forge.StaticSite {
        bucket = "gs://b"
        public_dir = "dist"
        keep_releases = 1
    }
}
