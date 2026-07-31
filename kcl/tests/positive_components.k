"""
Positive test — the Component schema hierarchy (Server / Worker / Cron /
Operator / Binary) expands to the right k8s manifests via
forge.components.render_components.

This constructs typed components directly (the `load_components` JSON
path is exercised by the scaffold e2e, which runs `kcl run
deploy/kcl/dev` against a real components_gen.json). Each `assert_*`
must evaluate true.
"""

import forge.components as fc

_components = [
    fc.Server {
        name = "api"
        ports = [
            fc.ComponentPort {name = "http", port = 8080, expose = True}
            fc.ComponentPort {name = "grpc", port = 9090, expose = True}
        ]
        env = {"LOG_LEVEL" = "info"}
    }
    fc.Worker {name = "sync"}
    fc.Cron {name = "reaper", schedule = "@hourly"}
    fc.Binary {name = "proxy", command = ["/app/demo", "proxy"]}
    fc.Operator {name = "controller", crds = ["Workspace"]}
]

_env = fc.ComponentEnv {
    namespace = "demo-dev"
    project = "demo"
    registry = "localhost:5050"
    image_tag = "latest"
    network_policies = False
}

manifests = fc.render_components(_components, _env)

# --- Shape assertions ---
_kinds = [m.kind for m in manifests]
assert_has_namespace = "Namespace" in _kinds
assert_has_deployment = "Deployment" in _kinds
assert_has_service = "Service" in _kinds
assert_has_cronjob = "CronJob" in _kinds

# Server -> Service with BOTH named ports.
_api_svc = [m for m in manifests if m.kind == "Service" and m.metadata.name == "api"][0]
_svc_port_names = [p.name for p in _api_svc.spec.ports]
assert_server_service_named_ports = "http" in _svc_port_names and "grpc" in _svc_port_names

# Server -> Deployment with both containerPorts.
_api_deploy = [m for m in manifests if m.kind == "Deployment" and m.metadata.name == "api"][0]
_container_ports = [p.name for p in _api_deploy.spec.template.spec.containers[0].ports]
assert_server_container_ports = "http" in _container_ports and "grpc" in _container_ports

# Worker -> Deployment, NO Service.
assert_worker_no_service = len([m for m in manifests if m.kind == "Service" and m.metadata.name == "sync"]) == 0

# Cron -> CronJob using the schedule.
_cron = [m for m in manifests if m.kind == "CronJob" and m.metadata.name == "reaper"][0]
assert_cron_schedule = _cron.spec.schedule == "@hourly"

# Binary -> Deployment running the cobra subcommand.
_bin = [m for m in manifests if m.kind == "Deployment" and m.metadata.name == "proxy"][0]
assert_binary_command = _bin.spec.template.spec.containers[0].command == ["/app/demo", "proxy"]
assert_binary_no_service = len([m for m in manifests if m.kind == "Service" and m.metadata.name == "proxy"]) == 0

# Operator -> manager Deployment + cluster-scoped RBAC (ClusterRole).
assert_operator_clusterrole = len([m for m in manifests if m.kind == "ClusterRole" and m.metadata.name == "controller-clusterrole"]) == 1
_op_deploy = [m for m in manifests if m.kind == "Deployment" and m.metadata.name == "controller"][0]
assert_operator_service_account = _op_deploy.spec.template.spec.serviceAccountName == "controller"

# Every workload binds its OWN ServiceAccount. Unbound, the pod runs as
# the namespace `default` SA — indistinguishable from every other
# workload in audit logs and in any identity-based policy.
_bound = [m.spec.template.spec.serviceAccountName for m in manifests if m.kind == "Deployment"]
assert_every_deployment_binds_an_sa = len(_bound) == 4 and len([s for s in _bound if not s]) == 0
_sa_names = [m.metadata.name for m in manifests if m.kind == "ServiceAccount"]
assert_bound_sas_exist = len([s for s in _bound if s not in _sa_names]) == 0

# No workload SA mounts an API token: forge Servers/Workers/Crons read
# their config from the container env, never from the API.
_workload_sas = [m for m in manifests if m.kind == "ServiceAccount" and m.metadata.name != "controller"]
assert_no_token_automount = len(_workload_sas) > 0 and \
    len([s for s in _workload_sas if s.automountServiceAccountToken != False]) == 0

# ...and none of them ships a Role granting blanket access to Secrets.
# A Role that reads every Secret in the namespace, bound to a workload
# that never calls the API, is pure blast radius dressed as security.
_workload_roles = [m for m in manifests if m.kind in ["Role", "ClusterRole"] and m.metadata.name != "controller-clusterrole"]
_secret_grants = [m for m in _workload_roles if len([r for r in m.rules if "secrets" in r.resources]) > 0]
assert_no_blanket_secret_reads = len(_secret_grants) == 0

# `app.kubernetes.io/part-of` names the PROJECT, not the component.
# A CD pipeline selects one release's objects with
# `-l app.kubernetes.io/part-of=<project>`; when every object labelled
# itself, that selector matched nothing and the post-deploy rollout /
# health loops iterated an EMPTY list and passed vacuously.
_labelled = [m for m in manifests if "labels" in m.metadata and m.metadata.labels]
assert_part_of_is_project = len(_labelled) > 0 and len([
    m for m in _labelled
    if "app.kubernetes.io/part-of" in m.metadata.labels and \
        m.metadata.labels["app.kubernetes.io/part-of"] != "demo"
]) == 0
# Pod templates carry the same label, so a Deployment's pods are
# selectable by project too.
assert_pod_template_part_of = _api_deploy.spec.template.metadata.labels["app.kubernetes.io/part-of"] == "demo"
# `app.kubernetes.io/name` still identifies the COMPONENT — the two
# labels must not collapse into one another.
assert_name_is_component = _api_deploy.metadata.labels["app.kubernetes.io/name"] == "api"
