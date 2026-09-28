"""
Negative test — ClusterTarget.registry is a REQUIRED input. An unset / typo'd
`-D registry=` that resolves to empty must fail at KCL LOAD, not produce an
ImagePullBackOff at deploy (a manifest whose image reference is missing its
registry).
"""

# expect: ClusterTarget.registry is required

import forge
import forge.workloads as fw

# A cloud target whose registry resolved to empty (the masked-default footgun
# without a consumer-side `or <default>`).
_prod = forge.ClusterTarget {
    cluster = "gke_proj_us-central1_prod"
    namespace = "app-prod"
    registry = ""
}

output = forge.render(forge.Bundle {
    project = "fixture"
    workloads = [fw.Workload {name = "api", image = "api", runtime = forge.OnCluster {target = _prod}}]
})
