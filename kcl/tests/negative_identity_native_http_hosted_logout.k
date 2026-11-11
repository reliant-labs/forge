"""
Negative test — an http POST-LOGOUT callback is refused on a hosted identity
too, not only the redirect: both lists reach the identity provider literally.
"""

# expect: accepted only by a host-run identity

import forge
import forge.workloads as fw

_prod = forge.ClusterTarget {cluster = "gke_acme_prod", namespace = "acme-prod", connected_cluster = "acme-prod", platform = "amd64"}

output = forge.render(forge.Bundle {
    project = "acme"
    env = "prod"
    control_plane = forge.ControlPlane {}
    cluster_target = _prod
    identity = forge.ManagedIdentity {applications = [forge.IdentityApplication {name = "m", kind = "native", frontend = "mobile", redirect_uris = ["com.acme.mobile:/oauthredirect"], logout_uris = ["http://localhost:8081/signedout"]}]}
    workloads = [fw.Workload {name = "api", image = "ghcr.io/acme/api", runtime = forge.OnCluster {target = _prod}}]
    frontends = [forge.Frontend {name = "mobile", path = "frontends/mobile", type = "rn", port = 8081, runtime = forge.OnHost {}}]
})
