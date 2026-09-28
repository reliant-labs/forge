"""
Negative test — `domains` on a frontend that is not OnHosted is REFUSED.

Only the platform verifies ownership of a hostname and issues its
certificate. Your own bucket and CDN serve an OnBucket site, so the domain
belongs where the certificate and the DNS record live — not in a field forge
would have to drop.
"""

# expect: only forge.OnHosted can serve a custom hostname
# expect: declare the domain there

import forge

output = forge.render(forge.Bundle {
    project = "fixture"
    frontends = [forge.Frontend {
        name = "web"
        path = "web"
        domains = ["hounders.club"]
        runtime = forge.OnBucket {bucket = "hounders-web"}
    }]
})
