"""
Positive test — per-service owned manifests (forge.RenderedWorkload.manifests) for
DECLARED-CLUSTER-ONLY routing.

Each manifest attached to a Service is rendered tagged with the owning
service's `app.kubernetes.io/name` label, so the deploy layer routes it to
that service's declared cluster (no primary-cluster heuristic). Two shapes
are exercised:

  1. A WORKLOAD service ("api", with an image) that also owns a CRD via
     `manifests` — the CRD is stamped with api's app label and the service
     still renders its Deployment + Service.

  2. An IMAGE-LESS "infra service" ("cp-infra", no image, manifests +
     K8sCluster deploy) that owns the env-level Gateway + a Namespace — those
     are stamped with cp-infra's label, and NO Deployment/Service/RBAC is
     rendered for cp-infra (the no-phantom-workload invariant).

Also covers the label-merge edge cases: an existing user label on a manifest
is PRESERVED (forge merges under it), and a manifest with NO metadata gets a
metadata.labels created.
"""

import forge

# Primary target — render_manifests reads namespace/registry off this one.
_target = forge.ClusterTarget {
    cluster = "k3d-control-plane"
    namespace = "control-plane-e2e"
    registry = "localhost:5051"
}

# A second cluster — one service rides this one (the multi-cluster split).
_daemon_target = forge.ClusterTarget {
    cluster = "k3d-cp-daemon"
    namespace = "control-plane-e2e"
    registry = "localhost:5051"
}

_bundle = forge.Bundle {
    cluster_target = _target
    services = [
        # 1. Workload service that ALSO owns a CRD. The CRD carries a
        # pre-existing user label to prove the merge preserves it.
        forge.RenderedWorkload {
            name = "api"
            image = "example"
            deploy = _target.deploy | {ports = [8080]}
            manifests = [
                {
                    apiVersion = "apiextensions.k8s.io/v1"
                    kind = "CustomResourceDefinition"
                    metadata = {
                        name = "widgets.example.com"
                        labels = {"team" = "platform"}
                    }
                    spec = {group = "example.com"}
                }
            ]
        }
        # 2. Image-less infra service: owns env-level resources pinned to
        # the control-plane cluster. One manifest deliberately has NO
        # metadata block to exercise the create-metadata merge path.
        forge.RenderedWorkload {
            name = "cp-infra"
            deploy = _target.deploy | {ports = [8080]}
            manifests = [
                {
                    apiVersion = "gateway.networking.k8s.io/v1"
                    kind = "Gateway"
                    metadata = {name = "public-gateway"}
                    spec = {gatewayClassName = "traefik"}
                }
                {
                    apiVersion = "cert-manager.io/v1"
                    kind = "ClusterIssuer"
                    spec = {acme = {}}
                }
            ]
        }
        # 3. A workload service on the OTHER cluster — proves the per-service
        # manifests mechanism is orthogonal to which cluster a service uses.
        forge.RenderedWorkload {
            name = "workspace-proxy"
            image = "example"
            deploy = _daemon_target.deploy
        }
    ]
}

manifests = forge.render_manifests(_bundle, "latest", {}, False)

# ── Helpers ────────────────────────────────────────────────────────
_crds = [m for m in manifests if m.kind == "CustomResourceDefinition"]
_gateways = [m for m in manifests if m.kind == "Gateway"]
_issuers = [m for m in manifests if m.kind == "ClusterIssuer"]
_deployments = [m for m in manifests if m.kind == "Deployment"]

# ── (1) Workload service owns a CRD, stamped with ITS label ────────
assert_crd_count = len(_crds) == 1
assert_crd_owner = _crds[0].metadata.labels["app.kubernetes.io/name"] == "api"
# The pre-existing user label is preserved (merge is non-destructive).
assert_crd_user_label = _crds[0].metadata.labels["team"] == "platform"
# The CRD's own metadata.name survives the metadata rebuild.
assert_crd_name = _crds[0].metadata.name == "widgets.example.com"
assert_crd_spec = _crds[0].spec.group == "example.com"

# ── (2) Infra service owns the Gateway + ClusterIssuer ─────────────
assert_gw_count = len(_gateways) == 1
assert_gw_owner = _gateways[0].metadata.labels["app.kubernetes.io/name"] == "cp-infra"
assert_gw_name = _gateways[0].metadata.name == "public-gateway"

# ClusterIssuer had NO metadata at all — the stamp creates metadata.labels.
assert_issuer_count = len(_issuers) == 1
assert_issuer_owner = _issuers[0].metadata.labels["app.kubernetes.io/name"] == "cp-infra"

# ── No phantom workload for the image-less infra service ───────────
# Only the two WORKLOAD services (api, workspace-proxy) render Deployments;
# cp-infra renders none.
_deploy_names = [d.metadata.name for d in _deployments]
assert_no_infra_deployment = len([n for n in _deploy_names if n == "cp-infra"]) == 0
assert_workload_deployments = len([n for n in _deploy_names if n == "api"]) == 1 \
    and len([n for n in _deploy_names if n == "workspace-proxy"]) == 1
