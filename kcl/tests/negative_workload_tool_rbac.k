"""
Negative test — a `tool` cannot declare RBAC.

A tool is built into the image and never scheduled, so it renders no
workload manifest at all. There is no pod, and therefore no
ServiceAccount for a Role to bind to.

Rejecting is the whole point rather than a formality. Silently accepting
the declaration would render nothing and report success, so the author
would be left believing a permission had been granted to something that
does not run — and would go looking for the failure anywhere except at
the declaration. The check names the alternative (`service` / `worker`)
because the author already knows what they wanted; the error's job is to
say where it goes.
"""

import forge.workloads as fw

_bad = fw.Workload {
    name = "admin-cli"
    kind = "tool"
    command = ["/app/demo", "admin"]
    namespaced_rbac = [
        {apiGroups = [""], resources = ["configmaps"], verbs = ["get"]}
    ]
}

manifests = fw.render_workloads([_bad], fw.WorkloadEnv {
    namespace = "demo-dev"
    network_policies = False
})
