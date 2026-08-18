"""
Negative test — forge.GitSource requires a `ref`.

forge deliberately does NOT default to a repository's default branch: an
unpinned cross-repo dependency is exactly what makes a build
unreproducible, which is the problem GitSource exists to solve. A source
with a repo and no ref must fail loudly rather than resolve to whatever
main happens to be today.

Expected: `kcl run` FAILS on the GitSource check.
"""

import forge

_fe = forge.Frontend {
    name = "reliant-web"
    type = "vite"
    source = forge.GitSource {
        repo = "github.com/reliant-labs/reliant"
        subdir = "web"
    }
}

output = forge.render(forge.Bundle { frontends = [_fe] })
