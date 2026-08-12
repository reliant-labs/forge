"""
Positive test — a one-shot Job's ORDERING is declared on the per-env
overlay, not discovered from the tree.

`before` is a DEPLOY fact: it is a relation between workloads in a
particular environment, and one env may legitimately gate a service the
way another does not (a job's own source says what it DOES, never what
must wait for it). So a base declaration in `workloads.k` may state no
`before`, and the env's main.k refines it on.

This test exercises that path: base workloads, with `before` layered on
via the `|` merge operator, the same way a real `deploy/kcl/<env>/main.k`
refines replicas and ports.
"""

import forge.workloads as fw

# The base shape a shared workloads.k declares: a job workload with a
# command and NO ordering.
_loaded = [
    {
        name = "api"
        kind = "service"
        ports = []
        env = {}
        command = []
        schedule = ""
        before = []
        group = ""
        version = ""
        crds = []
        build = {}
        image = ""
        registry = ""
        namespace = ""
        replicas = 1
        env_vars = []
        platform = ""
        leader_election = True
    }
    {
        name = "provision-idp"
        kind = "job"
        ports = []
        env = {}
        command = ["/app/demo", "provision-idp"]
        schedule = ""
        before = []
        group = ""
        version = ""
        crds = []
        build = {}
        image = ""
        registry = ""
        namespace = ""
        replicas = 1
        env_vars = []
        platform = ""
        leader_election = True
    }
]

# The per-env refinement: THIS environment gates `api` on the
# provisioning job. Another env may legitimately not.
_workloads = [c | {image = "demo"} | ({before = ["api"]} if c.name == "provision-idp" else {}) for c in _loaded]

_env = fw.WorkloadEnv {
    namespace = "demo-dev"
    project = "demo"
    registry = "localhost:5050"
    network_policies = False
}

manifests = fw.render_workloads(_workloads, _env)

# The overlaid ordering took effect: the job became an init container on
# the dependent, and rendered no standalone Job object.
_api = [m for m in manifests if m.kind == "Deployment" and m.metadata.name == "api"][0]
_inits = [c.name for c in _api.spec.template.spec.initContainers]
assert_overlay_ordering_applied = _inits == ["provision-idp"]
assert_no_standalone_job = len([m for m in manifests if m.kind == "Job"]) == 0
assert_init_runs_the_job = _api.spec.template.spec.initContainers[0].command == ["/app/demo", "provision-idp"]

# The compose lowering reads the same overlaid declaration.
_fragment = fw.compose_fragment(_workloads, "demo:latest")
assert_compose_gate = _fragment.services["api"].depends_on["provision-idp"].condition == "service_completed_successfully"
