"""
Positive test — the workload KIND × CAPABILITY matrix, asserted in BOTH
directions.

WHY BOTH DIRECTIONS. The defect this file guards is not "a capability is
broken"; it is "a capability is ABSENT and nobody can tell whether that
was on purpose". `render_worker` had no RBAC surface for exactly that
reason — it was written as `render_service` minus the Service object, and
minus'd the Role as well. Nothing failed at render time. The manifests
were valid, the Deployment rolled, and the first sweep of the worker's
reconcile loop failed in production with

    workspaces.reliant.dev is forbidden: User "system:serviceaccount:
    control-plane-prod:control-plane-workers" cannot list resource
    "workspaces" in API group "reliant.dev"

A test that only asserted "supported capabilities work" would have been
fully green the entire time. So each capability is pinned twice: the
kinds that HAVE it must render it, and the kinds that deliberately do NOT
must be shown to reject it (negative_workload_*.k) or to omit it. That is
what makes a future gap a failing test rather than a silent absence.

The matrix itself lives in kcl/workloads/capabilities.k, where each
kind's omissions are justified individually. This file pins that the
matrix and the RENDERERS agree — a table nothing reads would rot exactly
as fast as the hand-written expansions it replaced.
"""

import forge.workloads as fw

_env = fw.WorkloadEnv {
    namespace = "demo-dev"
    project = "demo"
    network_policies = False
    env = "dev"
}

_rules = [{apiGroups = ["reliant.dev"], resources = ["workspaces"], verbs = ["list", "watch"]}]

# ===========================================================================
# (1) THE REGRESSION ITSELF — a worker that declares namespaced_rbac gets a
#     real Role + RoleBinding. This is the assertion whose absence cost a
#     production outage.
# ===========================================================================
_worker = fw.Workload {
    name = "reaper"
    kind = "worker"
    namespaced_rbac = _rules
}
_worker_manifests = fw.render_workloads([_worker], _env)
_worker_kinds = [m.kind for m in _worker_manifests]

assert_worker_gets_role = "Role" in _worker_kinds
assert_worker_gets_rolebinding = "RoleBinding" in _worker_kinds
assert_worker_gets_sa = "ServiceAccount" in _worker_kinds
# A worker is NOT dialed by anything, so it must still get no Service —
# the one omission on this kind that IS deliberate.
assert_worker_gets_no_service = "Service" not in _worker_kinds

_worker_role = [m for m in _worker_manifests if m.kind == "Role"][0]
_worker_binding = [m for m in _worker_manifests if m.kind == "RoleBinding"][0]
_worker_deploy = [m for m in _worker_manifests if m.kind == "Deployment"][0]

# The declared rule is present, ADDITIVE to the config-read default — the
# same contract a service's Role has. A Role that REPLACED the defaults
# would break config reads while looking correct.
assert_worker_rule_present = {apiGroups = ["reliant.dev"], resources = ["workspaces"], verbs = ["list", "watch"]} in _worker_role.rules
assert_worker_default_config_read_still_present = {apiGroups = [""], resources = ["configmaps", "secrets"], verbs = ["get", "list", "watch"]} in _worker_role.rules
assert_worker_role_has_exactly_two_rules = len(_worker_role.rules) == 2
assert_worker_role_name = _worker_role.metadata.name == "reaper-role"

# The binding and the POD must name the same ServiceAccount. A Role bound
# to an SA no pod runs as is the "looks like scoped RBAC, grants nothing"
# shape lib/rbac.k exists to prevent — and it is indistinguishable from
# working RBAC in the rendered YAML unless something checks both ends.
assert_worker_binding_targets_sa = _worker_binding.subjects[0].name == "reaper"
assert_worker_pod_binds_that_sa = _worker_deploy.spec.template.spec.serviceAccountName == "reaper"

# ===========================================================================
# (2) EMPTY RULES STILL MEAN THE BARE IDENTITY. A worker that asks for
#     nothing gets a ServiceAccount with no Role — byte-identical to before
#     the capability existed. You get permission by ASKING for it; most
#     workloads never call the Kubernetes API at all, and a default Role
#     granting read on every Secret in the namespace would hand an RCE a
#     credential dump for no benefit.
# ===========================================================================
_plain_worker = fw.Workload {name = "plain", kind = "worker"}
_plain_kinds = [m.kind for m in fw.render_workloads([_plain_worker], _env)]
assert_plain_worker_gets_sa = "ServiceAccount" in _plain_kinds
assert_plain_worker_gets_no_role = "Role" not in _plain_kinds
assert_plain_worker_gets_no_rolebinding = "RoleBinding" not in _plain_kinds

# ===========================================================================
# (3) EVERY namespaced-RBAC-CAPABLE KIND behaves identically. The bug was
#     that one kind (`job`) had the capability and the others did not, which
#     made the gap look considered. Pinning all four together is what stops
#     a future kind being wired into some renderers and not others.
# ===========================================================================
_rbac_kinds_manifests = fw.render_workloads([
    fw.Workload {name = "svc", kind = "service", namespaced_rbac = _rules}
    fw.Workload {name = "wrk", kind = "worker", namespaced_rbac = _rules}
    fw.Workload {name = "crn", kind = "cron", schedule = "@hourly", namespaced_rbac = _rules}
    fw.Workload {name = "job", kind = "job", command = ["/app/x", "seed"], namespaced_rbac = _rules}
], _env)
_role_names = [m.metadata.name for m in _rbac_kinds_manifests if m.kind == "Role"]

assert_service_supports_namespaced_rbac = "svc-role" in _role_names
assert_worker_supports_namespaced_rbac = "wrk-role" in _role_names
assert_cron_supports_namespaced_rbac = "crn-role" in _role_names
assert_job_supports_namespaced_rbac = "job-role" in _role_names

# ===========================================================================
# (4) OPERATOR — cluster RBAC, and the rules it DECLARES.
#
#     render_operator passed a hardcoded `{rules = []}`, so the one kind
#     that by definition watches the API server across namespaces could not
#     state what it watches. Every real operator therefore had to supply its
#     ClusterRole out of band, as a raw manifest with the SAME NAME as the
#     near-empty one forge generated — two objects competing for one name,
#     where whichever applied last won.
# ===========================================================================
_operator = fw.Workload {
    name = "ctl"
    kind = "operator"
    crds = ["Workspace"]
    cluster_rbac = _rules
    ports = [fw.Port {name = "metrics", port = 8080}]
}
_op_manifests = fw.render_workloads([_operator], _env)
_op_kinds = [m.kind for m in _op_manifests]
_op_clusterrole = [m for m in _op_manifests if m.kind == "ClusterRole"][0]

assert_operator_gets_clusterrole = "ClusterRole" in _op_kinds
assert_operator_gets_clusterrolebinding = "ClusterRoleBinding" in _op_kinds
assert_operator_declared_rule_present = {apiGroups = ["reliant.dev"], resources = ["workspaces"], verbs = ["list", "watch"]} in _op_clusterrole.rules
assert_operator_default_config_read_present = {apiGroups = [""], resources = ["configmaps", "secrets"], verbs = ["get", "list", "watch"]} in _op_clusterrole.rules

# THE TIERS ARE EXCLUSIVE. An operator gets a ClusterRole INSTEAD OF a Role,
# never both: two bindings on one ServiceAccount means the wider one decides
# every outcome, so the narrower Role would describe permissions that are not
# the permissions in force, and deleting it would change nothing.
assert_operator_gets_no_namespaced_role = "Role" not in _op_kinds
assert_operator_gets_no_rolebinding = "RoleBinding" not in _op_kinds

# An operator's declared ports reach its containerPorts. `render_operator`
# passed an EMPTY port list, so a manager that declared :8080 metrics or
# :9443 for a webhook got a manifest that never mentioned them — the same
# silent-drop class as the missing RBAC, and invisible for the same reason
# (containerPort is documentary, so nothing failed; the manifest just
# stopped describing the process). control-plane's workspace-controller
# declares port 9191 and had been rendering without it.
#
# This cell of the matrix was ALSO got wrong on the first pass — written as
# "operators have no ports" — and was caught by rendering a real project
# against the matrix rather than by reasoning about it. That is worth
# recording: the matrix is a claim about behaviour, and claims need a
# consumer to check them.
_op_container = [m for m in _op_manifests if m.kind == "Deployment"][0].spec.template.spec.containers[0]
assert_operator_declared_port_reaches_container = _op_container.ports[0].containerPort == 8080
assert_operator_port_keeps_its_name = _op_container.ports[0].name == "metrics"

# But NO Service object. The asymmetry with kind="service" is deliberate:
# a containerPort is documentary and always safe to state, whereas a
# Service is a routable name other workloads resolve — forge should not
# conjure one for a manager on the strength of a port number.
assert_operator_gets_no_service_object = "Service" not in _op_kinds

# ===========================================================================
# (5) CRON IS A POD, AND MUST SAY SO.
#
#     forge's own Namespace enforces `restricted` Pod Security, and the cron
#     pod carried no securityContext — so the API server rejected its pods
#     at CREATION. The CronJob object applied cleanly and `kubectl get
#     cronjob` looked healthy; the failure appeared only on the first tick,
#     as a FailedCreate event on the Job that nothing was watching.
#
#     It also never bound the ServiceAccount it emitted (so the pod ran as
#     the namespace `default`), and dropped `resources` (BestEffort — the
#     first thing evicted under node pressure, which for a sweeper means the
#     sweep silently did not happen).
# ===========================================================================
_cron_manifests = fw.render_workloads([
    fw.Workload {name = "sweep", kind = "cron", schedule = "@hourly", namespaced_rbac = _rules}
], _env)
_cron_pod = [m for m in _cron_manifests if m.kind == "CronJob"][0].spec.jobTemplate.spec.template.spec
_cron_container = _cron_pod.containers[0]

assert_cron_pod_has_security_context = _cron_pod.securityContext.runAsNonRoot == True
assert_cron_pod_seccomp_is_runtime_default = _cron_pod.securityContext.seccompProfile.type == "RuntimeDefault"
assert_cron_container_has_security_context = _cron_container.securityContext.readOnlyRootFilesystem == True
assert_cron_container_drops_all_caps = _cron_container.securityContext.capabilities.drop == ["ALL"]
assert_cron_binds_its_service_account = _cron_pod.serviceAccountName == "sweep"
assert_cron_container_has_resources = _cron_container.resources.requests.cpu == "100m"
assert_cron_container_has_limits = _cron_container.resources.limits.memory == "512Mi"

# ===========================================================================
# (6) DELIBERATE OMISSIONS — the half a "does it work" test never covers.
#
#     Each of these is a capability a kind does NOT have ON PURPOSE. They are
#     asserted so that "absent" is a recorded decision rather than an
#     accident waiting to be discovered the way the worker's RBAC was.
# ===========================================================================

# A tool is built into the image and NEVER scheduled, so it renders no
# workload manifest at all. Everything else here follows from that.
_tool_manifests = fw.render_workloads([
    fw.Workload {name = "cli", kind = "tool", command = ["/app/demo", "admin"]}
], _env)
_tool_kinds = [m.kind for m in _tool_manifests]
assert_tool_renders_no_deployment = "Deployment" not in _tool_kinds
assert_tool_renders_no_service = "Service" not in _tool_kinds
assert_tool_renders_no_rbac = "Role" not in _tool_kinds and "ClusterRole" not in _tool_kinds
assert_tool_renders_no_service_account = "ServiceAccount" not in _tool_kinds
# The Namespace is env-shared and belongs to no workload, so it is the only
# thing a tool-only environment renders.
assert_tool_only_env_renders_just_the_namespace = _tool_kinds == ["Namespace"]

# Batch kinds get no PodDisruptionBudget: a PDB protects a long-running
# replica set from a node drain, and there is nothing long-running here.
_pdb_names = [m.metadata.name for m in _rbac_kinds_manifests if m.kind == "PodDisruptionBudget"]
assert_cron_gets_no_pdb = "crn-pdb" not in _pdb_names
assert_job_gets_no_pdb = "job-pdb" not in _pdb_names

# Probes are for kinds that serve HTTP health endpoints BY CONSTRUCTION. A
# cron pod runs to completion, so a liveness probe would restart a job that
# is merely still working, and a readiness probe would gate traffic that
# never arrives.
assert_cron_container_has_no_liveness_probe = "livenessProbe" not in _cron_container
assert_cron_container_has_no_readiness_probe = "readinessProbe" not in _cron_container

# ===========================================================================
# (7) THE MATRIX AND THE RENDERERS ARE THE SAME TABLE.
#
#     The derived kind lists must come FROM the matrix, not sit beside it.
#     They were hand-maintained lists in two files, which is how a kind could
#     be added to one and not the other.
# ===========================================================================
assert_worker_is_a_deployment_kind = "worker" in fw.DEPLOYMENT_KINDS
assert_tool_is_not_a_deployment_kind = "tool" not in fw.DEPLOYMENT_KINDS
assert_every_kind_has_a_capability_row = len(fw.WORKLOAD_KINDS) == len(fw.KIND_CAPABILITIES)
# A broadcast job gates what a release rolls out. `cron` is excluded because
# a CronJob pod fires on a SCHEDULE rather than as part of a rollout —
# gating it would re-run the one-shot on every tick, forever.
assert_broadcast_gates_deployment_kinds = "worker" in fw.BROADCAST_GATED_KINDS and "service" in fw.BROADCAST_GATED_KINDS
assert_broadcast_does_not_gate_cron = "cron" not in fw.BROADCAST_GATED_KINDS
assert_broadcast_does_not_gate_tool = "tool" not in fw.BROADCAST_GATED_KINDS

# The `supports` predicate is what both the schema checks and the renderers
# consult, so it is pinned directly rather than only through its effects.
assert_supports_worker_namespaced_rbac = fw.supports("worker", fw.CAP_NAMESPACED_RBAC)
assert_supports_operator_cluster_rbac = fw.supports("operator", fw.CAP_CLUSTER_RBAC)
assert_operator_does_not_support_namespaced_rbac = not fw.supports("operator", fw.CAP_NAMESPACED_RBAC)
assert_tool_supports_nothing = len(fw.KIND_CAPABILITIES["tool"]) == 0
# An unknown kind supports nothing rather than erroring — Workload.check
# already rejects a bad kind with a message naming the valid ones, and a
# second error from the matrix would fire first and report the capability
# instead of the typo that caused it.
assert_unknown_kind_supports_nothing = not fw.supports("nonesuch", fw.CAP_NAMESPACED_RBAC)

# The RBAC tier is a single function because the tiers are EXCLUSIVE, and
# that exclusivity is the property worth centralizing.
assert_worker_rbac_tier_is_namespaced = fw.rbac_tier("worker") == "namespaced"
assert_operator_rbac_tier_is_cluster = fw.rbac_tier("operator") == "cluster"
assert_tool_rbac_tier_is_none = fw.rbac_tier("tool") == "none"
