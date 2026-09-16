"""
Negative test — SimpleBackend rejects network = "none" together with ports.

"none" emits NO Service object (see positive_simple_backend_network.k), so
a port declared here describes an address that does not exist: nothing can
dial it, by name or otherwise. Accepting the combination silently would
make the two fields disagree with no way for the author to tell which one
won — the declaration says "listens on 8080", the cluster says "not
addressable", and both are true.

Rejecting it forces the author to say which they meant.
"""

import forge

_none_with_ports = forge.RenderedWorkload {
    name = "worker"
    deploy = forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        image = "ghcr.io/acme/worker:v1"
        network = "none"
        ports = [8080]
    }
}

_bundle = forge.Bundle {
    cluster_target = forge.ClusterTarget {
        cluster = "hosted"
        namespace = "acme-prod"
        registry = "ghcr.io/reliant-labs"
    }
    services = [_none_with_ports]
}

output = forge.render(_bundle)
