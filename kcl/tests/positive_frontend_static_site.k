"""
Positive test — a Frontend with a StaticSite deploy target renders into
the expected JSON shape: bucket / public_dir / base_path, the bundle
list, the ordered cache_control rules, the CDN block with its
invalidation policy, and keep_releases.

Mirrors the shape the Go StaticSiteProvider consumes (see
internal/deploytarget/staticsite.go): a Next.js static export mounted
under /admin with a sibling Vite SPA bundled at the site root, immutable
caching on content-hashed assets and must-revalidate on everything else.
"""

import forge

_fe = forge.Frontend {
    name = "admin-web"
    type = "nextjs"
    path = "admin-web"
    env_vars = [forge.EnvVar { name = "NEXT_PUBLIC_API_URL", value = "https://api.staging.example.com" }]
    deploy = forge.StaticSite {
        bucket = "gs://reliant-staging-web"
        public_dir = "out"
        base_path = "/admin"
        bundle = [forge.BundleDir { src = "../reliant-web/dist", dest = "" }]
        cache_control = [
            forge.CacheRule { pattern = "_next/static/**", cache_control = "public, max-age=31536000, immutable" }
            forge.CacheRule { pattern = "**", cache_control = "public, max-age=0, must-revalidate" }
        ]
        cdn = forge.StaticSiteCDN {
            url_map = "reliant-staging-lb"
            extra_invalidate_paths = ["/manifest.json"]
        }
        keep_releases = 5
    }
}

_bundle = forge.Bundle {
    frontends = [_fe]
}

output = forge.render(_bundle)

# ── Invariants ─────────────────────────────────────────────────────

_d = output.frontends[0].deploy

assert_deploy_present = _d != None
assert_deploy_type = _d.type == "static-site"
assert_deploy_bucket = _d.bucket == "gs://reliant-staging-web"
assert_deploy_public_dir = _d.public_dir == "out"
assert_deploy_base_path = _d.base_path == "/admin"

# Bundles use the target-neutral BundleDir and project identically to the
# Firebase path (see positive_frontend_firebase.k's bundle assertions).
assert_bundle_count = len(_d.bundle) == 1
assert_bundle_src = _d.bundle[0].src == "../reliant-web/dist"
assert_bundle_dest_root = _d.bundle[0].dest == ""

# cache_control keeps DECLARATION ORDER — first match wins at upload
# time, so a reordered projection would silently apply the catch-all to
# the immutable assets.
assert_cache_count = len(_d.cache_control) == 2
assert_cache_first_pattern = _d.cache_control[0].pattern == "_next/static/**"
assert_cache_first_value = _d.cache_control[0].cache_control == "public, max-age=31536000, immutable"
assert_cache_last_pattern = _d.cache_control[1].pattern == "**"

# CDN block, with `invalidate` defaulting to the rate-limit-safe
# "entrypoints" rather than a blanket purge.
assert_cdn_present = _d.cdn != None
assert_cdn_url_map = _d.cdn.url_map == "reliant-staging-lb"
assert_cdn_invalidate_default = _d.cdn.invalidate == "entrypoints"
assert_cdn_extra_paths = _d.cdn.extra_invalidate_paths[0] == "/manifest.json"

assert_keep_releases = _d.keep_releases == 5

# Build-time env_vars round-trip on the frontend itself, exactly as they
# do for a Firebase frontend — the build half is shared.
assert_envvar_name = output.frontends[0].env_vars[0].name == "NEXT_PUBLIC_API_URL"
assert_envvar_value = output.frontends[0].env_vars[0].value == "https://api.staging.example.com"
