"""
Positive test — a one-shot `job` that declares `namespaced_rbac` gets a
REAL Role + RoleBinding, not the bare identity-only ServiceAccount every
other job gets.

WHY THIS EXISTS. Most one-shots never touch the Kubernetes API at all — a
migration job talks to postgres, not to the cluster — so `render_job` gave
every job the same bare SA a Server/Worker/Cron/Binary gets: no Role, no
mounted token (see lib/rbac.k::render_service_account). That was correct
for every job forge had ever scaffolded, until an IdP-provisioning job
needed to WRITE a ConfigMap in its own namespace to publish the values it
converges (a generated OIDC client_id) for other workloads to read via
configMapKeyRef — see the `auth/dev-loop` skill's IdP-convergence job.

A job that needs cluster-API access is now able to say so exactly the way
a `service` already can: `namespaced_rbac`, the same PolicyRule-dict shape
as `Service.namespaced_rbac` in the parent forge package. Declaring it
switches the job from the bare-identity ServiceAccount to a real
Role + RoleBinding carrying the default config-read rules PLUS whatever
this job asked for.

What this pins:

  1. A job with NO `namespaced_rbac` renders the bare SA — byte-identical
     to every job that existed before this field, e.g. the scaffolded
     `migrate` step.
  2. A job that DOES declare `namespaced_rbac` gets a Role (default rules
     + its own) and a RoleBinding, and the Job's pod binds that
     ServiceAccount.
  3. The extra rules are genuinely ADDITIVE to the default config-read
     rules, not a replacement — the same contract Service.namespaced_rbac
     has via render_namespaced_rbac.
"""

import forge.workloads as fw

_env = fw.WorkloadEnv {
    namespace = "demo-dev"
    project = "demo"
    network_policies = False
}

# --- (1) no namespaced_rbac declared: bare SA, same as before this field ---
_plain_job = fw.Workload {
    kind = "job"
    name = "migrate"
    command = ["/app/demo", "db", "migrate", "up"]
    image = "demo"
}
_plain_manifests = fw.render_workloads([_plain_job], _env)
_plain_kinds = [m.kind for m in _plain_manifests]
assert_plain_job_gets_bare_sa = "ServiceAccount" in _plain_kinds
assert_plain_job_gets_no_role = "Role" not in _plain_kinds
assert_plain_job_gets_no_rolebinding = "RoleBinding" not in _plain_kinds

# --- (2) namespaced_rbac declared: real Role + RoleBinding, SA bound ---
_idp_job = fw.Workload {
    kind = "job"
    name = "idp-provision"
    command = ["/app/demo", "auth", "idp-provision"]
    image = "demo"
    namespaced_rbac = [
        {apiGroups = [""], resources = ["configmaps"], verbs = ["get", "create", "update", "patch"]}
    ]
}
_rbac_manifests = fw.render_workloads([_idp_job], _env)
_rbac_kinds = [m.kind for m in _rbac_manifests]
assert_rbac_job_gets_sa = "ServiceAccount" in _rbac_kinds
assert_rbac_job_gets_role = "Role" in _rbac_kinds
assert_rbac_job_gets_rolebinding = "RoleBinding" in _rbac_kinds

_role = [m for m in _rbac_manifests if m.kind == "Role"][0]
_rolebinding = [m for m in _rbac_manifests if m.kind == "RoleBinding"][0]
_job_obj = [m for m in _rbac_manifests if m.kind == "Job"][0]

# --- (3) additive: default config-read rules + the job's own ---
assert_default_config_read_rule_still_present = {resources = ["configmaps", "secrets"], verbs = ["get", "list", "watch"], apiGroups = [""]} in _role.rules
assert_own_configmap_write_rule_present = {apiGroups = [""], resources = ["configmaps"], verbs = ["get", "create", "update", "patch"]} in _role.rules
assert_role_has_exactly_two_rules = len(_role.rules) == 2
assert_role_name = _role.metadata.name == "idp-provision-role"
assert_rolebinding_binds_the_jobs_sa = _rolebinding.subjects[0].name == "idp-provision"
assert_job_pod_binds_that_sa = _job_obj.spec.template.spec.serviceAccountName == "idp-provision"
