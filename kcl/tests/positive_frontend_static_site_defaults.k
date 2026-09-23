"""
Positive test — a MINIMAL StaticSite (bucket + public_dir only) renders
its defaults explicitly rather than dropping the keys.

The load-bearing one is `keep_releases = 0`: it must survive the
projection as 0 ("retain everything"), not vanish and be read back as
the default 10. A retention policy that silently flips from "never
delete" to "keep 10" would delete archived releases the author asked
forge to keep — and it would do it in a bucket, where the loss is not
recoverable.

Also pins that an absent `cdn` projects as null (bucket served directly,
no invalidation) and that an omitted base_path means the site root.
"""

import forge

_fe = forge.Frontend {
    name = "web"
    type = "vite"
    path = "web"
    deploy = forge.StaticSite {
        bucket = "reliant-preview-web"
        public_dir = "dist"
        keep_releases = 0
    }
}

output = forge.render(forge.Bundle { frontends = [_fe] })

_d = output.frontends[0].deploy

assert_type = _d.type == "static-site"
# A bare bucket name (no gs:// scheme) is accepted verbatim; the Go
# consumer normalizes it.
assert_bucket_bare = _d.bucket == "reliant-preview-web"
assert_public_dir = _d.public_dir == "dist"

# Omitted base_path → null → the site root.
assert_base_path_null = _d.base_path == None

# Empty collections project as empty lists, not null, so the consumer
# never has to distinguish "absent" from "none declared".
assert_bundle_empty = len(_d.bundle) == 0
assert_cache_empty = len(_d.cache_control) == 0

# No CDN declared → null. Distinct from a declared CDN with
# invalidate = "none".
assert_cdn_null = _d.cdn == None

# THE ONE THAT MATTERS: 0 survives as 0.
assert_keep_releases_zero_survives = _d.keep_releases == 0
