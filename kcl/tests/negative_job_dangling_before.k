"""
Negative test — a Job whose `before` names a component that does not
exist in the environment is rejected at LOAD time.

Without this check the failure is silent and worse than a crash: the job
renders, nothing gates on it, and the ordering the author declared simply
does not happen. The dependent starts against an unprovisioned world and
fails somewhere else entirely, at runtime, in a message that names none of
this.
"""

import forge.workloads as fw

_components = [
    fw.Workload {kind = "service", name = "api"}
    fw.Workload {
        kind = "job"
        name = "provision"
        command = ["/app/demo", "provision"]
        # `apiserver` does not exist — the component is called `api`.
        before = ["apiserver"]
    }
]

_env = fw.WorkloadEnv {
    namespace = "demo-dev"
    project = "demo"
    network_policies = False
}

manifests = fw.render_workloads(_components, _env)
