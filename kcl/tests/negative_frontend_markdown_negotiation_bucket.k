"""
Negative test — markdown_negotiation on a frontend a bucket serves.

Negotiation varies a page on the Accept header; a bucket serves one object
per URL, so the declaration has no carrier there and is refused, naming the
runtime, rather than dropped.
"""

# expect: markdown_negotiation
# expect: bucket
# expect: forge.OnHosted

import forge

output = forge.render(forge.Bundle {
    project = "fixture"
    frontends = [forge.Frontend {
        name = "web"
        path = "web"
        type = "vite"
        markdown_negotiation = True
        runtime = forge.OnBucket {bucket = "gs://acme-web"}
    }]
})
