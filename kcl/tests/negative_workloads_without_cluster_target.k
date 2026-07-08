"""
Negative test — a Bundle.workloads entry (agnostic forge.Service) WITHOUT a
cluster_target must fail at LOAD with a clear check message, not a confusing
projection error deep inside `_service_to_k8s` (where `target.deploy`
dereferences an Undefined ClusterTarget).

Agnostic workloads are cluster-only today: the k8s adapter seeds each from the
env-wide ClusterTarget. Omitting it is an author error the Bundle check catches
up front.
"""

import forge

_svc = forge.Service {
    name = "orphan"
    image = "orphan-img"
    ports = [8080]
}

# No cluster_target — the check must reject this.
_bundle = forge.Bundle {
    workloads = [_svc]
}

output = forge.render(_bundle)
