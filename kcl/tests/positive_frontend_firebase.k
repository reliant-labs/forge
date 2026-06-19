"""
Positive test — a Frontend with a FirebaseHosting deploy target renders
into the expected JSON shape: the deploy block carries the
project/site/target/public_dir/base_path, the bundle list, and rewrites;
build-time env_vars round-trip alongside it.

Mirrors the real reliant-web + admin-web recipe: a Next.js static export
mounted under /admin, with a sibling Vite SPA bundled at the site root.
"""

import forge

_fe = forge.Frontend {
    name = "admin-web"
    type = "nextjs"
    path = "admin-web"
    env_vars = [forge.EnvVar { name = "NEXT_PUBLIC_API_URL", value = "https://api.staging.example.com" }]
    deploy = forge.FirebaseHosting {
        project = "reliant-nonprod-490701"
        site = "reliant-staging"
        public_dir = "out"
        base_path = "/admin"
        bundle = [forge.FirebaseBundleDir { src = "../reliant-web/dist", dest = "" }]
        rewrites = [{ source = "**", destination = "/index.html" }]
    }
}

_bundle = forge.Bundle {
    frontends = [_fe]
}

output = forge.render(_bundle)

# ── Invariants ─────────────────────────────────────────────────────

_d = output.frontends[0].deploy

assert_deploy_present = _d != None
assert_deploy_type = _d.type == "firebase"
assert_deploy_project = _d.project == "reliant-nonprod-490701"
assert_deploy_site = _d.site == "reliant-staging"
# target omitted -> projects as null (consumer defaults it to site).
assert_deploy_target_null = _d.target == None
assert_deploy_public_dir = _d.public_dir == "out"
assert_deploy_base_path = _d.base_path == "/admin"
assert_deploy_bundle_count = len(_d.bundle) == 1
assert_deploy_bundle_src = _d.bundle[0].src == "../reliant-web/dist"
assert_deploy_rewrites = _d.rewrites[0].destination == "/index.html"

# Build-time env_vars round-trip on the frontend itself.
assert_envvar_name = output.frontends[0].env_vars[0].name == "NEXT_PUBLIC_API_URL"
assert_envvar_value = output.frontends[0].env_vars[0].value == "https://api.staging.example.com"
