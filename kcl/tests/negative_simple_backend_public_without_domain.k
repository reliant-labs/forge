"""
Negative test — SimpleBackend rejects network = "public" with no domain.

`domain` is what the platform's ingress keys on; it is the ONLY thing a
public backend contributes beyond a private one (the Service object is
identical). A public backend with no hostname therefore declares an
intent forge can render nothing for — it would come out byte-identical
to a private one while its author believed it was reachable from the
internet.

The mirror check is asserted too, in positive_simple_backend_network.k:
a domain on a private/"none" backend is REJECTED rather than ignored,
because a hostname served by nothing is the same lie in the other
direction.
"""

import forge

_public_no_domain = forge.RenderedWorkload {
    name = "web"
    deploy = forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        image = "ghcr.io/acme/web:v1"
        ports = [8080]
        network = "public"
    }
}

_bundle = forge.Bundle {
    cluster_target = forge.ClusterTarget {
        cluster = "hosted"
        namespace = "acme-prod"
        registry = "ghcr.io/reliant-labs"
    }
    services = [_public_no_domain]
}

output = forge.render(_bundle)
