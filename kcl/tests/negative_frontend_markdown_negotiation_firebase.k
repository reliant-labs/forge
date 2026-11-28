"""
Negative test — markdown_negotiation on a frontend Firebase serves.

Firebase Hosting serves one file per URL and cannot vary on Accept.
"""

# expect: markdown_negotiation
# expect: firebase
# expect: Firebase serves one file per URL

import forge

output = forge.render(forge.Bundle {
    project = "fixture"
    frontends = [forge.Frontend {
        name = "web"
        path = "web"
        type = "vite"
        markdown_negotiation = True
        runtime = forge.OnFirebase {project = "acme", site = "acme-web"}
    }]
})
