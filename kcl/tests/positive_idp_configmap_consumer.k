"""
Positive test — a workload consumes the identity a deploy-time job
PUBLISHED, by referencing the ConfigMap BY NAME, with no render-time
resolution at all.

THE DESIGN THIS PINS. The old resolve_oidc_client KCL plugin dialed the
IdP mid-render, from inside `forge generate`. The convergence design that
replaced it (see the `auth/dev-loop` skill) never needs a render to LEARN
a generated value: an idp-provision Job converges the registration once
and publishes what it generated into a ConfigMap the job's own KCL
declares by name (IDPProvisionConfigMapName in Go); a consumer just
references that name via `config_map_ref`, exactly like any other
declared config source — see kcl/tests/positive_workload_env_channels.k
for the general channel this reuses.

There is nothing forge-specific to test in the LOOKUP itself (that is
covered generically already); what is specific here is that the SAME
ConfigMap name a consumer writes by hand is the one the job's own RBAC
grant (positive_job_namespaced_rbac.k) lets it write to — i.e. the two
declarations, written independently by a human reading two different
files, actually name the same object.
"""

import forge.workloads as fw

_CONFIGMAP_NAME = "demo-idp-identity"

# The job that converges and publishes the identity.
_idp_job = fw.Workload {
    kind = "job"
    name = "idp-provision"
    command = ["/app/demo", "auth", "idp-provision", "--configmap-name", _CONFIGMAP_NAME]
    image = "demo"
    namespaced_rbac = [
        {apiGroups = [""], resources = ["configmaps"], verbs = ["get", "create", "update", "patch"]}
    ]
}

# A consumer that reads the client id and audience the job publishes,
# by NAME — no plugin call, no render-time IdP dial.
_admin_server = fw.Workload {
    kind = "service"
    name = "admin-server"
    image = "demo"
    ports = [fw.Port {name = "http", port = 8080, expose = True}]
    env_vars = [
        {name = "OIDC_CLIENT_ID", config_map_ref = _CONFIGMAP_NAME, config_map_key = "client_id"}
        {name = "JWT_AUDIENCE", config_map_ref = _CONFIGMAP_NAME, config_map_key = "audience"}
    ]
}

_env = fw.WorkloadEnv {
    namespace = "demo-dev"
    project = "demo"
    network_policies = False
}

_manifests = fw.render_workloads([_idp_job, _admin_server], _env)

# --- the consumer's env resolves via configMapKeyRef, naming the SAME
#     ConfigMap the job's RBAC grant lets it write to ---
_deploy = [m for m in _manifests if m.kind == "Deployment" and m.metadata.name == "admin-server"][0]
_container = _deploy.spec.template.spec.containers[0]
_by_name = {e.name: e for e in _container.env}

assert_client_id_reads_from_published_configmap = _by_name["OIDC_CLIENT_ID"].valueFrom.configMapKeyRef == {name = _CONFIGMAP_NAME, key = "client_id"}
assert_audience_reads_from_published_configmap = _by_name["JWT_AUDIENCE"].valueFrom.configMapKeyRef == {name = _CONFIGMAP_NAME, key = "audience"}

# --- the job's Role grants exactly the write that ConfigMap needs, under
#     its OWN identity — the RBAC and the reference agree on the name by
#     construction, not by luck ---
_role = [m for m in _manifests if m.kind == "Role" and m.metadata.name == "idp-provision-role"][0]
assert_job_role_grants_configmap_write = {apiGroups = [""], resources = ["configmaps"], verbs = ["get", "create", "update", "patch"]} in _role.rules

# --- no plugin, no render-time dependency: this whole file evaluates with
#     ONLY `import forge.workloads`, which is the point — nothing here
#     imports kcl_plugin.forge ---
