"""
Negative test — ClusterTarget.registry is a REQUIRED input. An unset / typo'd
`-D registry=` that resolves to empty must fail at KCL LOAD, not produce an
ImagePullBackOff at deploy (a manifest whose image reference is missing its
registry).
"""

import forge

# A cloud target whose registry resolved to empty (the masked-default footgun
# without a consumer-side `or <default>`).
_prod = forge.ClusterTarget {
    cluster = "gke_proj_us-central1_prod"
    namespace = "app-prod"
    registry = ""
}

output = forge.render(forge.Bundle {
    cluster_target = _prod
    services = []
})
