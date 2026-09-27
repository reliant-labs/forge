"""
Negative test — Workload.namespaced_rbac on a GATING job (one with `before`)
is refused.

A gating job renders no Job and no identity: it is an initContainer inside
each dependent's pod, so it runs under the DEPENDENT's ServiceAccount. A Role
granted to the job would bind nothing, and the job would run without the
rules it asked for — so the declaration is an error, not a silent no-op.
"""

import forge.workloads as fw

_j = fw.Workload {
    kind = "job"
    name = "idp-provision"
    command = ["/app", "auth", "idp-provision"]
    before = [fw.BEFORE_ALL]
    namespaced_rbac = [
        {apiGroups = [""], resources = ["configmaps"], verbs = ["get", "create", "update", "patch"]}
    ]
}
