"""
Positive test — the Workload schema expands to the right k8s manifests
via forge.workloads.render_workloads, one kind at a time.

This constructs workloads directly — the same way a project's own
`deploy/kcl/workloads.k` does. The end-to-end path (a scaffolded
project's workloads.k + per-env refinement) is exercised by the scaffold
e2e, which runs `kcl run deploy/kcl/dev`. Each `assert_*` must evaluate
true.
"""

import forge.workloads as fw

_workloads = [
    fw.Workload {
        name = "api"
        kind = "service"
        ports = [
            fw.Port {name = "http", port = 8080, expose = True}
            fw.Port {name = "grpc", port = 9090, expose = True}
        ]
        env = {"LOG_LEVEL" = "info"}
    }
    fw.Workload {name = "sync", kind = "worker"}
    fw.Workload {name = "reaper", kind = "cron", schedule = "@hourly"}
    fw.Workload {name = "proxy", kind = "tool", command = ["/app/demo", "proxy"]}
    fw.Workload {name = "controller", kind = "operator", crds = ["Workspace"]}
]

_env = fw.WorkloadEnv {
    namespace = "demo-dev"
    project = "demo"
    registry = "localhost:5050"
    image_tag = "latest"
    network_policies = False
    # The env-ownership tag. Stated explicitly here (the KCL drill-in)
    # because this fixture runs with no `-D env=` binding; in a real
    # render the field defaults to that binding. See lib/labels.k.
    env = "dev"
}

manifests = fw.render_workloads(_workloads, _env)

# --- Shape assertions ---
_kinds = [m.kind for m in manifests]
assert_has_namespace = "Namespace" in _kinds
assert_has_deployment = "Deployment" in _kinds
assert_has_service = "Service" in _kinds
assert_has_cronjob = "CronJob" in _kinds

# service -> Service with BOTH named ports.
_api_svc = [m for m in manifests if m.kind == "Service" and m.metadata.name == "api"][0]
_svc_port_names = [p.name for p in _api_svc.spec.ports]
assert_service_named_ports = "http" in _svc_port_names and "grpc" in _svc_port_names

# service -> Deployment with both containerPorts.
_api_deploy = [m for m in manifests if m.kind == "Deployment" and m.metadata.name == "api"][0]
_container_ports = [p.name for p in _api_deploy.spec.template.spec.containers[0].ports]
assert_service_container_ports = "http" in _container_ports and "grpc" in _container_ports

# worker -> Deployment, NO Service.
assert_worker_no_service = len([m for m in manifests if m.kind == "Service" and m.metadata.name == "sync"]) == 0

# cron -> CronJob using the schedule.
_cron = [m for m in manifests if m.kind == "CronJob" and m.metadata.name == "reaper"][0]
assert_cron_schedule = _cron.spec.schedule == "@hourly"

# tool -> NOTHING. A tool is built into the image and never scheduled, so
# it has no runtime manifest at all: no Deployment, no Service, no SA.
# This is the load-bearing difference from the `binary` kind it replaced,
# which rendered an unscheduled Deployment nobody ever addressed.
assert_tool_no_deployment = len([m for m in manifests if m.metadata.name == "proxy"]) == 0

# operator -> manager Deployment + cluster-scoped RBAC (ClusterRole).
assert_operator_clusterrole = len([m for m in manifests if m.kind == "ClusterRole" and m.metadata.name == "controller-clusterrole"]) == 1
_op_deploy = [m for m in manifests if m.kind == "Deployment" and m.metadata.name == "controller"][0]
assert_operator_service_account = _op_deploy.spec.template.spec.serviceAccountName == "controller"

# Every workload binds its OWN ServiceAccount. Unbound, the pod runs as
# the namespace `default` SA — indistinguishable from every other
# workload in audit logs and in any identity-based policy.
_bound = [m.spec.template.spec.serviceAccountName for m in manifests if m.kind == "Deployment"]
assert_every_deployment_binds_an_sa = len(_bound) == 3 and len([s for s in _bound if not s]) == 0
_sa_names = [m.metadata.name for m in manifests if m.kind == "ServiceAccount"]
assert_bound_sas_exist = len([s for s in _bound if s not in _sa_names]) == 0

# No workload SA mounts an API token: forge services/workers/crons read
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

# `app.kubernetes.io/part-of` names the PROJECT, not the workload.
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
# `app.kubernetes.io/name` still identifies the WORKLOAD — the two
# labels must not collapse into one another.
assert_name_is_workload = _api_deploy.metadata.labels["app.kubernetes.io/name"] == "api"

# Every object this path emits carries the env-ownership stamp
# (`forge.dev/env`), including the Namespace and the pod templates — the
# workloads renderer goes through the same lib/labels.k gate as
# forge.render_manifests. Selectors are NOT touched: the Deployment's
# matchLabels stays the exact one-key form it has always been, which is
# what keeps `kubectl apply` from failing "field is immutable" on an
# existing Deployment.
_env_key = "forge.dev/env"
assert_env_label_on_every_object = len([
    m for m in manifests if m.metadata.labels[_env_key] != "dev"
]) == 0
assert_env_label_on_pod_template = \
    _api_deploy.spec.template.metadata.labels[_env_key] == "dev"
assert_env_label_leaves_selector_alone = \
    _api_deploy.spec.selector.matchLabels == {"app.kubernetes.io/name" = "api"}
