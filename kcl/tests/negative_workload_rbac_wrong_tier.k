"""
Negative test — a `worker` cannot declare CLUSTER-scoped RBAC.

The tiers are not "more permission" vs "less"; they are different scopes,
and a workload gets one or the other. A `service` or `worker` that
genuinely watches resources across namespaces IS an operator — declaring
it as one is what gets it leader election and the CRD wiring too.
"""

# expect: clusterRBAC is only supported for kind='operator'

import forge.workloads as fw

_bad = fw.Workload {
    name = "sweeper"
    kind = "worker"
    clusterRBAC = [
        {apiGroups = ["reliant.dev"], resources = ["workspaces"], verbs = ["list", "watch"]}
    ]
}
