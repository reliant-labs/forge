"""
ACCEPTANCE TEST for the per-service cluster override (`Service.k8s.cluster`) —
the multi-cluster split where ONE workload lands in a DIFFERENT cluster than
the Bundle's `cluster_target` (control-plane's workspace-proxy in a SEPARATE
`cp-daemon` cluster).

A Bundle carries two workloads:
  * `admin` — plain, no override → the Bundle's cluster_target (the app cluster).
  * `proxy` — `k8s.cluster = <daemon target>` → the OTHER cluster.

The k8s adapter projects `proxy` onto the override target's coordinates
(namespace / registry / cluster context / image_pull_secrets), and the manifest
render lands its Deployment/Service in the OTHER namespace with the OTHER
registry. `admin` is unchanged. This mirrors the outcome the old per-service
`deploy = forge.K8sCluster{...}` group attribution produced.
"""

import forge

# The Bundle's default target — the app cluster.
_app = forge.ClusterTarget {
    cluster = "k3d-control-plane"
    namespace = "control-plane-prod"
    registry = "ghcr.io/reliant-labs/app"
    image_pull_secrets = ["app-pull"]
}

# The OTHER target — the daemon cluster the proxy is pinned to.
_daemon = forge.ClusterTarget {
    cluster = "k3d-cp-daemon"
    namespace = "cp-daemon-prod"
    registry = "ghcr.io/reliant-labs/daemon"
    image_pull_secrets = ["daemon-pull"]
}

# Plain workload — no override, rides the Bundle cluster_target.
_admin = forge.Service {
    name = "admin"
    image = "control-plane"
    ports = [8090]
}

# Pinned workload — targets the daemon cluster via k8s.cluster.
_proxy = forge.Service {
    name = "workspace-proxy"
    image = "control-plane"
    ports = [8080]
    k8s = forge.K8sOverrides {
        cluster = _daemon
    }
}

_bundle = forge.Bundle {
    cluster_target = _app
    workloads = [_admin, _proxy]
}

# ── JSON contract: each workload carries its OWN cluster coordinates. ──
output = forge.render(_bundle)
_svc_by_name = {s.name: s for s in output.services}

# admin → Bundle target (app cluster).
assert_admin_cluster = _svc_by_name["admin"].deploy.cluster == "k3d-control-plane"
assert_admin_namespace = _svc_by_name["admin"].deploy.namespace == "control-plane-prod"
assert_admin_registry = _svc_by_name["admin"].deploy.registry == "ghcr.io/reliant-labs/app"

# proxy → OVERRIDE target (daemon cluster).
assert_proxy_cluster = _svc_by_name["workspace-proxy"].deploy.cluster == "k3d-cp-daemon"
assert_proxy_namespace = _svc_by_name["workspace-proxy"].deploy.namespace == "cp-daemon-prod"
assert_proxy_registry = _svc_by_name["workspace-proxy"].deploy.registry == "ghcr.io/reliant-labs/daemon"
assert_proxy_pull_secrets = _svc_by_name["workspace-proxy"].deploy.image_pull_secrets == ["daemon-pull"]

# ── Manifest render: the Deployment/Service actually land in the OTHER
#    namespace with the OTHER registry (the real deploy artifact). ──
manifests = forge.render_manifests(_bundle, "v1", {}, False)
_deploys = {m.metadata.name: m for m in manifests if m.kind == "Deployment"}
_svcs = {m.metadata.name: m for m in manifests if m.kind == "Service"}

# admin — unchanged (app cluster namespace + registry).
assert_admin_deploy_ns = _deploys["admin"].metadata.namespace == "control-plane-prod"
assert_admin_image = _deploys["admin"].spec.template.spec.containers[0].image == "ghcr.io/reliant-labs/app/control-plane:v1"

# proxy — the OTHER cluster's namespace + registry.
assert_proxy_deploy_ns = _deploys["workspace-proxy"].metadata.namespace == "cp-daemon-prod"
assert_proxy_svc_ns = _svcs["workspace-proxy"].metadata.namespace == "cp-daemon-prod"
assert_proxy_image = _deploys["workspace-proxy"].spec.template.spec.containers[0].image == "ghcr.io/reliant-labs/daemon/control-plane:v1"

# The proxy's RBAC (ServiceAccount) also lands in the override namespace and
# carries the override pull secret — the coordinates flow through wholesale.
_sas = {m.metadata.name: m for m in manifests if m.kind == "ServiceAccount"}
assert_proxy_sa_ns = _sas["workspace-proxy"].metadata.namespace == "cp-daemon-prod"
assert_proxy_sa_pull = _sas["workspace-proxy"].imagePullSecrets == [{name = "daemon-pull"}]
assert_admin_sa_pull = _sas["admin"].imagePullSecrets == [{name = "app-pull"}]
