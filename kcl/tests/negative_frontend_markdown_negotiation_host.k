"""
Negative test — markdown_negotiation on a frontend bound to its dev server.

The dev server serves the HTML page for every request; only the hosted
origin negotiates. Refused, naming the runtime.
"""

# expect: markdown_negotiation
# expect: host
# expect: dev server

import forge

output = forge.render(forge.Bundle {
    project = "fixture"
    frontends = [forge.Frontend {
        name = "web"
        path = "web"
        type = "vite"
        markdown_negotiation = True
        runtime = forge.OnHost {}
    }]
})
