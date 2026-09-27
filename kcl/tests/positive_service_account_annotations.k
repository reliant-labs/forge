"""
Positive test — `service_account_annotations` land on the GENERATED
ServiceAccount, and on nothing else.

The case: GKE Workload Identity reads `iam.gke.io/gcp-service-account` off
the KSA a pod runs as. control-plane cannot get there with
`K8sOverrides.service_account` (bind an out-of-band SA), because its live
leader-election and PDP RBAC bind the literal workload-named SAs
(`admin-server`, `workspace-controller`, `workspace-proxy`) — renaming the
identity breaks those grants. So the annotation has to go on the SA forge
already generates, keeping its name.

Pinned, for all three authoring surfaces (agnostic Service via K8sOverrides,
hand-authored RenderedWorkload, forge.workloads.Workload) and both RBAC tiers:

  * the generated SA carries exactly the declared annotations;
  * the SA's NAME is unchanged, and the pod still binds it (the identity is
    the same one RBAC refers to);
  * the annotation is NOT on the Deployment, the pod template, the Role or
    the RoleBinding — the pod must not grow an annotation that would roll it
    or mean something else to another controller;
  * a workload declaring none renders an SA with NO `annotations` key
    (byte-identical to before) — the positive control.
"""

import forge
import forge.workloads as fw

_WI = "iam.gke.io/gcp-service-account"
_GSA = "deploy-publisher@reliant-labs-475814.iam.gserviceaccount.com"

_prod = forge.ClusterTarget {
    cluster = "prod-cluster"
    namespace = "control-plane-prod"
    registry = "ghcr.io/reliant-labs"
}

_bundle = forge.Bundle {
    cluster_target = _prod
    workloads = [
        # Namespaced tier, authored as an agnostic Service.
        forge.Service {
            name = "admin-server"
            image = "control-plane"
            ports = [8090]
            k8s = forge.K8sOverrides {
                service_account_annotations = {"${_WI}" = _GSA}
                namespaced_rbac = [
                    {apiGroups = ["coordination.k8s.io"], resources = ["leases"], verbs = ["get", "update"]}
                ]
                # Pod annotations stay independent of SA annotations.
                annotations = {"prometheus.io/scrape" = "true"}
            }
        }
        # Cluster (operator) tier, authored as an agnostic Service.
        forge.Service {
            name = "workspace-controller"
            image = "control-plane"
            ports = [9191]
            k8s = forge.K8sOverrides {
                service_account_annotations = {"${_WI}" = "static-site-writer@p.iam.gserviceaccount.com"}
                operator = forge.OperatorSpec {
                    cluster_rbac = [{apiGroups = ["reliant.dev"], resources = ["workspaces"], verbs = ["get", "list", "watch"]}]
                    crds = ["Workspace"]
                }
            }
        }
        # POSITIVE CONTROL: no annotations declared.
        forge.Service {
            name = "plain"
            image = "control-plane"
            ports = [8080]
        }
    ]
    services = [
        # Hand-authored RenderedWorkload carrying the field directly.
        forge.RenderedWorkload {
            name = "raw"
            image = "raw-img"
            deploy = _prod.deploy | {ports = [8080]}
            service_account_annotations = {"eks.amazonaws.com/role-arn" = "arn:aws:iam::1:role/raw", "plain-key" = "v"}
        }
    ]
}

_manifests = forge.render_manifests(_bundle, "abc1234", {}, False)

_of = lambda kind: str, name: str -> any {
    [m for m in _manifests if m.kind == kind and m.metadata.name == name][0]
}

_admin_sa = _of("ServiceAccount", "admin-server")
_admin_dep = _of("Deployment", "admin-server")
_ctrl_sa = _of("ServiceAccount", "workspace-controller")
_ctrl_dep = _of("Deployment", "workspace-controller")
_plain_sa = _of("ServiceAccount", "plain")
_raw_sa = _of("ServiceAccount", "raw")

# ── the annotation lands on the generated SA ──────────────────────────
assert_admin_sa_annotated = _admin_sa.metadata.annotations == {"${_WI}" = _GSA}
assert_ctrl_sa_annotated = _ctrl_sa.metadata.annotations == {"${_WI}" = "static-site-writer@p.iam.gserviceaccount.com"}
assert_raw_sa_annotated = _raw_sa.metadata.annotations == {"eks.amazonaws.com/role-arn" = "arn:aws:iam::1:role/raw", "plain-key" = "v"}

# ── the identity is unchanged: same SA name, still bound by the pod ───
assert_admin_pod_binds_annotated_sa = _admin_dep.spec.template.spec.serviceAccountName == _admin_sa.metadata.name
assert_admin_sa_keeps_workload_name = _admin_sa.metadata.name == "admin-server"
assert_ctrl_pod_binds_annotated_sa = _ctrl_dep.spec.template.spec.serviceAccountName == _ctrl_sa.metadata.name
assert_admin_binding_subject = _of("RoleBinding", "admin-server-rolebinding").subjects[0].name == "admin-server"
assert_ctrl_binding_subject = _of("ClusterRoleBinding", "workspace-controller-control-plane-prod-clusterrolebinding").subjects[0].name == "workspace-controller"

# ── ...and NOWHERE else ───────────────────────────────────────────────
# Every rendered object other than the three annotated SAs is free of the
# workload-identity keys, at object level AND on any pod template.
_annotated_sas = ["admin-server", "workspace-controller", "raw"]
_wi_keys = [_WI, "eks.amazonaws.com/role-arn", "plain-key"]
_obj_ann = lambda m: any -> {str: str} {
    m.metadata.annotations if m.metadata and m.metadata.annotations else {}
}
_tmpl_ann = lambda m: any -> {str: str} {
    (m.spec.template.metadata.annotations or {}) if m.kind == "Deployment" else {}
}
_carries_wi = lambda m: any -> bool {
    len([k for k in _wi_keys if k in _obj_ann(m) or k in _tmpl_ann(m)]) > 0
}
_leaks = [
    "${m.kind}/${m.metadata.name}"
    for m in _manifests
    if not (m.kind == "ServiceAccount" and m.metadata.name in _annotated_sas) and _carries_wi(m)
]
# The leak scan is only meaningful if it would SEE an annotated object:
# the three annotated SAs themselves must register as carrying the keys.
assert_leak_scan_sees_annotated_sas = len([m for m in _manifests if m.kind == "ServiceAccount" and _carries_wi(m)]) == 3
assert_no_leak_to_other_objects = _leaks == []
# Pod annotations are their own, independent field.
assert_pod_annotations_independent = _admin_dep.spec.template.metadata.annotations == {"prometheus.io/scrape" = "true"}

# ── POSITIVE CONTROL: none declared => no annotations key at all ──────
assert_plain_sa_has_no_annotations = "annotations" not in _plain_sa.metadata

# ── forge.workloads path: every tier's generated SA carries them ──────
_env = fw.WorkloadEnv {namespace = "demo-dev", project = "demo", network_policies = False}
_wl_manifests = fw.render_workloads([
    # Bare-identity tier (no rules): render_service_account.
    fw.Workload {name = "api", service_account_annotations = {"${_WI}" = "api@p.iam.gserviceaccount.com"}}
    # Namespaced tier.
    fw.Workload {
        name = "reaper"
        kind = "worker"
        namespaced_rbac = [{apiGroups = ["reliant.dev"], resources = ["workspaces"], verbs = ["list"]}]
        service_account_annotations = {"${_WI}" = "reaper@p.iam.gserviceaccount.com"}
    }
    # Cluster tier.
    fw.Workload {
        name = "ctl"
        kind = "operator"
        crds = ["Workspace"]
        service_account_annotations = {"${_WI}" = "ctl@p.iam.gserviceaccount.com"}
    }
    # Control.
    fw.Workload {name = "bare"}
], _env)
_wl_sa = lambda n: str -> any {
    [m for m in _wl_manifests if m.kind == "ServiceAccount" and m.metadata.name == n][0]
}
assert_wl_bare_tier = _wl_sa("api").metadata.annotations == {"${_WI}" = "api@p.iam.gserviceaccount.com"}
assert_wl_namespaced_tier = _wl_sa("reaper").metadata.annotations == {"${_WI}" = "reaper@p.iam.gserviceaccount.com"}
assert_wl_cluster_tier = _wl_sa("ctl").metadata.annotations == {"${_WI}" = "ctl@p.iam.gserviceaccount.com"}
assert_wl_control = "annotations" not in _wl_sa("bare").metadata
_wl_leaks = [
    m.metadata.name for m in _wl_manifests
    if m.kind != "ServiceAccount" and _WI in _obj_ann(m)
]
assert_wl_no_leak = _wl_leaks == []

# ── key validation accepts the real-world shapes ──────────────────────
import forge.lib.annotations as annotations_lib
assert_valid_gke = annotations_lib.valid_key("iam.gke.io/gcp-service-account")
assert_valid_eks = annotations_lib.valid_key("eks.amazonaws.com/role-arn")
assert_valid_bare = annotations_lib.valid_key("a")
assert_valid_mixed = annotations_lib.valid_key("Some_Key.v1-x")
assert_invalid_two_slashes = not annotations_lib.valid_key("a/b/c")
assert_invalid_empty = not annotations_lib.valid_key("")
assert_invalid_trailing_dash = not annotations_lib.valid_key("name-")
assert_invalid_upper_prefix = not annotations_lib.valid_key("IAM.gke.io/x")
assert_invalid_empty_prefix = not annotations_lib.valid_key("/x")
assert_invalid_space = not annotations_lib.valid_key("has space")
assert_invalid_long_name = not annotations_lib.valid_key("x" * 64)
assert_valid_63_name = annotations_lib.valid_key("x" * 63)
