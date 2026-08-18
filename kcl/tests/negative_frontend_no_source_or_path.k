"""
Negative test — a Frontend must declare a source of code.

`path` became optional so `source` could replace it; that must not turn
into "neither is required". A frontend with no code location is a build
that has nothing to build.

Expected: `kcl run` FAILS on the Frontend check.
"""

import forge

_fe = forge.Frontend {
    name = "orphan-web"
    type = "nextjs"
}

output = forge.render(forge.Bundle { frontends = [_fe] })
