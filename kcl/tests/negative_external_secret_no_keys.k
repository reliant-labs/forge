"""
Negative test — forge.ExternalSecret requires at least one data key. An
ExternalSecret with an empty `keys` list models a prerequisite the
preflight can't verify (no key to check), so it's rejected at parse time.
"""

import forge

_bundle = forge.Bundle {
    cluster_target = forge.ClusterTarget {
        cluster = "prod", namespace = "ns", registry = "ghcr.io/x"
    }
    required_secrets = [
        forge.ExternalSecret {
            name = "cloudflare-api-token"
            namespace = "cert-manager"
            keys = []
        }
    ]
}

output = forge.render(_bundle)
