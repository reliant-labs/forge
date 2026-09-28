"""
Negative test — a workload cannot declare BOTH RBAC tiers.

clusterRBAC is allowed on every scheduled kind, but the tiers are
EXCLUSIVE: a workload gets a namespaced Role or a ClusterRole, never both.
Two bindings on one ServiceAccount means the wider one decides every
outcome, so the narrower Role would describe permissions that are not the
permissions in force — and deleting it would change nothing.
"""

# expect: namespacedRBAC and clusterRBAC are exclusive

import forge.workloads as fw

_bad = fw.Workload {
    name = "sweeper"
    kind = "worker"
    args = ["sweep"]
    namespacedRBAC = [{apiGroups = [""], resources = ["configmaps"], verbs = ["get"]}]
    clusterRBAC = [{apiGroups = ["reliant.dev"], resources = ["workspaces"], verbs = ["list", "watch"]}]
}
