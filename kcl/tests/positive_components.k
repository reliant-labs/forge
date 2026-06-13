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
