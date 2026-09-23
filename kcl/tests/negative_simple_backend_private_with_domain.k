"""
Negative test — SimpleBackend rejects a domain on a non-public backend.

The mirror of negative_simple_backend_public_without_domain.k. A private
(or "none") backend is served by no ingress, so a hostname declared here
resolves to nothing and is honoured by nothing. Ignoring it silently
would be worse than rejecting it: the author reads their own declaration
back as evidence the backend is published, and the only way to discover
otherwise is that requests fail.

Rejecting both directions is what makes `domain` mean one thing.
"""

import forge

_private_with_domain = forge.RenderedWorkload {
    name = "internal-api"
    deploy = forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        image = "ghcr.io/acme/internal:v1"
        ports = [9000]
        network = "private"
        domain = "internal-acme.reliantapps.dev"
    }
}

_bundle = forge.Bundle {
    cluster_target = forge.ClusterTarget {
        cluster = "hosted"
        namespace = "acme-prod"
        registry = "ghcr.io/reliant-labs"
    }
    services = [_private_with_domain]
}

output = forge.render(_bundle)
