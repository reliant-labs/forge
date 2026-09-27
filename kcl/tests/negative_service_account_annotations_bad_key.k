"""
NEGATIVE: a `service_account_annotations` key that is not a legal Kubernetes
annotation key must fail the render, not the apply.

The apiserver would reject the ServiceAccount mid-deploy, after other
objects in the same stream were already applied, naming the manifest rather
than the KCL field. The K8sOverrides check rejects it at declaration.
"""

import forge

_svc = forge.Service {
    name = "admin-server"
    image = "control-plane"
    k8s = forge.K8sOverrides {
        # Two slashes: not `[prefix/]name`.
        service_account_annotations = {"iam.gke.io/gcp/service-account" = "x@p.iam.gserviceaccount.com"}
    }
}
