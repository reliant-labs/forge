"""
Positive test — a frontend bound to forge.OnFirebase.

The build and the assembly are the frontend's (public_dir, base_path,
bundle); the runtime names the Firebase project/site and carries rewrites
verbatim. `target` omitted projects as null (the consumer defaults it to the
site). Mirrors the reliant-web + admin-web recipe: a Next.js export under
/admin with a sibling Vite SPA bundled at the site root.
"""

import forge

output = forge.render(forge.Bundle {
    project = "fixture"
    frontends = [forge.Frontend {
        name = "admin-web"
        type = "nextjs"
        path = "admin-web"
        public_dir = "out"
        base_path = "/admin"
        bundle = [forge.BundleDir {src = "../reliant-web/dist"}]
        runtime = forge.OnFirebase {
            project = "reliant-nonprod-490701"
            site = "reliant-staging"
            rewrites = [{source = "**", destination = "/index.html"}]
        }
    }]
})

_f = output.frontends[0]
assert_type = _f.runtime.type == "firebase"
assert_project = _f.runtime.project == "reliant-nonprod-490701"
assert_site = _f.runtime.site == "reliant-staging"
assert_target_null = _f.runtime.target == None
assert_rewrites = _f.runtime.rewrites[0].destination == "/index.html"
assert_public_dir = _f.public_dir == "out"
assert_base_path = _f.base_path == "/admin"
assert_bundle = _f.bundle[0].src == "../reliant-web/dist"
