"""
Negative test — SimpleBackend rejects a resource shape off the 4 GiB-per
-vCPU band.

This is the check that exists for a BILLING reason rather than a
correctness one, so it is worth being explicit about what it prevents.
The platform does not reject a lopsided request — control-plane's
inframeter.RatioFloor bills the MAX of (what you asked for, what the band
implies on each axis). So 1000m + 512Mi is not refused, it is silently
REPRICED to 1000m + 4Gi: the app owner asked for half a gibibyte and is
billed for four, with nothing at deploy time having said so.

Rejecting the declaration at `kcl run` is what makes the invoice equal
the request. It fires during `forge env render` / `build` / `deploy`,
before anything touches a cluster — the same discipline as the
ClusterTarget.registry check, which refuses at load rather than letting
kubelet discover the problem on a pull.

The check lives on SimpleBackend and NOT on forge.Resources, deliberately:
Resources is shared by every workload forge renders, and a self-hosted
K8sCluster service must not inherit a hosted tier's pricing rule.
"""

import forge

_lopsided = forge.RenderedWorkload {
    name = "cpu-heavy"
    deploy = forge.SimpleBackend {
        cluster = "hosted"
        namespace = "acme-prod"
        image = "ghcr.io/acme/app:v1"
        ports = [8080]
        # 1000m implies 4Gi at the 4 GiB/vCPU band; 512Mi is an eighth of
        # that. The platform would bill this as 1000m + 4Gi.
        resources = forge.Resources {
            cpu_request_millicores = 1000
            cpu_limit_millicores = 1000
            memory_request_bytes = forge.mib(512)
            memory_limit_bytes = forge.mib(512)
        }
    }
}

_bundle = forge.Bundle {
    cluster_target = forge.ClusterTarget {
        cluster = "hosted"
        namespace = "acme-prod"
        registry = "ghcr.io/reliant-labs"
    }
    services = [_lopsided]
}

output = forge.render(_bundle)
