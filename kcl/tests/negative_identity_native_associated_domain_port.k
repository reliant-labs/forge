"""
Negative test — an association file is fetched from the exact host, so a port is refused.
"""

# expect: bare lowercase hostnames

import forge
import forge.workloads as fw

_prod = forge.ClusterTarget {cluster = "gke_acme_prod", namespace = "acme-prod", connected_cluster = "acme-prod", platform = "amd64"}

output = forge.render(forge.Bundle {
    project = "acme"
    env = "prod"
    control_plane = forge.ControlPlane {}
    cluster_target = _prod
    identity = forge.ManagedIdentity {applications = [forge.IdentityApplication {name = "m", kind = "native", frontend = "mobile", associated_domains = ["app.acme.example:8443"]}]}
    workloads = [fw.Workload {name = "api", image = "ghcr.io/acme/api", runtime = forge.OnCluster {target = _prod}}]
    frontends = [forge.Frontend {name = "mobile", path = "frontends/mobile", type = "rn", port = 8081, runtime = forge.OnHost {}}]
})
