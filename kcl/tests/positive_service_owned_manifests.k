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
import yaml

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
        # 4. Chart-shaped owned manifests whose OWN app.kubernetes.io/* labels
        # disagree with forge's stamp (control-plane's kata pre-pull and its
        # vendored CNPG render). The DaemonSet's name label is what its
        # selector keys on, so it must survive; the decoded doc's
        # `managed-by: Helm` used to be a KCL unification error that failed
        # the whole render.
        forge.RenderedWorkload {
            name = "prepull"
            deploy = _daemon_target.deploy
            manifests = [
                {
                    apiVersion = "apps/v1"
                    kind = "DaemonSet"
                    metadata = {
                        name = "image-prepull"
                        namespace = "prepull"
                        labels = {"app.kubernetes.io/name" = "image-prepull"}
                    }
                    spec = {
                        selector.matchLabels = {"app.kubernetes.io/name" = "image-prepull"}
                        template.metadata.labels = {"app.kubernetes.io/name" = "image-prepull"}
                    }
                }
                yaml.decode("apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: operator\n  namespace: prepull\n  labels:\n    app.kubernetes.io/name: operator-chart\n    app.kubernetes.io/managed-by: Helm\n    forge.dev/workload: someone-else\n    helm.sh/chart: operator-1.0.0\n")
            ]
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

# ── (4) The owner is authoritative; the manifest's own labels survive ──
# forge.dev/workload is the routing key --target and the cluster scoper read
# (cluster.ManifestGroup), so it names the OWNER on every owned manifest.
_owned = _crds + _gateways + _issuers
assert_owner_key_everywhere = all m in _owned {
    m.metadata.labels["forge.dev/workload"] == m.metadata.labels["app.kubernetes.io/name"]
}
_ds = [m for m in manifests if m.kind == "DaemonSet"][0]
_sa = [m for m in manifests if m.kind == "ServiceAccount" and m.metadata.name == "operator"][0]
assert_ds_routed_to_owner = _ds.metadata.labels["forge.dev/workload"] == "prepull"
# ...without touching the name label its selector depends on.
assert_ds_name_label_kept = _ds.metadata.labels["app.kubernetes.io/name"] == "image-prepull"
assert_ds_selector_untouched = _ds.spec.selector.matchLabels == {"app.kubernetes.io/name" = "image-prepull"}
# The decoded chart doc renders (no unification error) with its own values
# kept, forge's routing key forced, and absent defaults filled in.
assert_decoded_routed_to_owner = _sa.metadata.labels["forge.dev/workload"] == "prepull"
assert_decoded_managed_by_kept = _sa.metadata.labels["app.kubernetes.io/managed-by"] == "Helm"
assert_decoded_name_kept = _sa.metadata.labels["app.kubernetes.io/name"] == "operator-chart"
assert_decoded_other_label_kept = _sa.metadata.labels["helm.sh/chart"] == "operator-1.0.0"
assert_decoded_part_of_defaulted = _sa.metadata.labels["app.kubernetes.io/part-of"] == "prepull"
