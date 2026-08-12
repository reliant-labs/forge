"""
Negative test — an environment cannot refine a VALID base workload into
an INVALID one. `kcl run` must FAIL.

This is the other half of positive_workload_inheritance.k, and the
reason a base declaration is a schema instance rather than a plain
value: `|` preserves the schema type, so forge.workloads' `check:` rules
re-run on the MERGED result. The error surfaces in the environment that
made the bad override, naming the field.

A plain-dict base would type as `{str:any}`, merge silently, and render
a Deployment with `replicas: 0` — a workload that deploys successfully
and serves nothing.
"""

import forge.workloads as fw

# A complete, VALID base declaration — exactly what workloads.k states.
api = fw.Workload {
    name = "api"
    kind = "service"
    ports = [fw.Port {name = "http", port = 8080, expose = True}]
}

# The environment refines it into something invalid. `Workload.replicas
# must be positive` fires HERE, on the merged value.
_prod = api | {replicas = 0}

manifests = fw.render_workloads([_prod], fw.WorkloadEnv {
    namespace = "demo-prod"
    project = "demo"
    network_policies = False
})
