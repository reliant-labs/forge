"""
NEGATIVE: the forge.workloads surface validates `service_account_annotations`
keys too — an empty prefix (`/name`) is not a legal annotation key.
"""

import forge.workloads as fw

manifests = fw.render_workloads([
    fw.Workload {
        name = "api"
        service_account_annotations = {"/gcp-service-account" = "api@p.iam.gserviceaccount.com"}
    }
], fw.WorkloadEnv {namespace = "demo-dev", network_policies = False})
