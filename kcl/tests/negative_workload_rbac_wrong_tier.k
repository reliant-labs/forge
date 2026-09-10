"""
Negative test — a `worker` cannot declare CLUSTER-scoped RBAC.

The tiers are not "more permission" vs "less"; they are different scopes,
and a workload gets one or the other. A `service` or `worker` that
genuinely watches resources across namespaces IS an operator — declaring
it as one is what gets it leader election and the CRD wiring too.

The alternative to rejecting this is rendering a Deployment whose author
wrote down cluster permissions it does not have. That is the exact shape
of the outage this whole matrix exists to prevent: valid manifests, a pod
that starts, and a failure on the first reconcile sweep.
"""

import forge.workloads as fw

_bad = fw.Workload {
    name = "sweeper"
    kind = "worker"
    cluster_rbac = [
        {apiGroups = ["reliant.dev"], resources = ["workspaces"], verbs = ["list", "watch"]}
    ]
}

manifests = fw.render_workloads([_bad], fw.WorkloadEnv {
    namespace = "demo-dev"
    network_policies = False
})
