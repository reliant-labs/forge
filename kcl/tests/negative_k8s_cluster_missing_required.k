"""
Negative test — K8sCluster requires cluster, namespace, and registry.
Missing any of them should fail KCL's check block.
"""

import forge

_svc = forge.RenderedWorkload {
    name = "bad"
    deploy = forge.K8sCluster {
        # cluster, namespace, registry all missing
    }
}
