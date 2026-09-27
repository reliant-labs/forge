"""
NEGATIVE: `service_account_annotations` together with a `service_account`
override must fail.

The override means forge generates NO ServiceAccount (it does not own an SA
it was told already exists), so the annotations would have nowhere to land.
Silently dropping them is the failure this refuses: the render would look
configured while the pod ran with no cloud identity.
"""

import forge

_svc = forge.Service {
    name = "admin-server"
    image = "control-plane"
    k8s = forge.K8sOverrides {
        service_account = "reliant-cloudsql"
        service_account_annotations = {"iam.gke.io/gcp-service-account" = "x@p.iam.gserviceaccount.com"}
    }
}
