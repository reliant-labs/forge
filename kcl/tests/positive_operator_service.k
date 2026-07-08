"""
Positive test — the OPERATOR-agnostic model (OPERATOR_AGNOSTIC.md): an
operator is NOT a distinct top-level type. It is an ordinary agnostic
`forge.Service` whose `k8s` escape hatch carries a cluster-scoped
`operator` sub-block (OperatorSpec: cluster_rbac + crds). Setting that
sub-block MARKS the Service an operator: the k8s adapter emits a
cluster-scoped ClusterRole + ClusterRoleBinding (not the namespaced Role a
plain Service gets), and the Service's `ports` generate the `Service`
object the operator's RPC listener needs.

Models control-plane's real `workspace-controller` — the last component
still authored as the legacy `forge.Operator`:

  * it is a controller-runtime manager that reconciles the `Workspace` CRD
    (crds = ["Workspace"], 12-rule cluster RBAC incl. leases), AND
  * it ALSO serves a Connect RPC on :9191 — which the legacy `Operator`
    could NOT express (no `ports`), forcing control-plane to hand-write a
    SECOND `Service` manifest and triplicate the :9191 literal.

The WIN this fixture pins: authored as a `forge.Service` with `ports=[9191]`
+ `k8s.operator`, ONE core spec yields BOTH the ClusterRole/Binding AND the
:9191 Service — deleting the hand-written `workspace_controller_svc`.

Asserts:
  (a) the Deployment renders;
  (b) a Service on :9191 is emitted FROM `ports` (the win);
  (c) a ClusterRole `workspace-controller-clusterrole` + its
      ClusterRoleBinding are emitted with the given rules (name matches
      control-plane's admin-server rebind assumption, lib/rbac.k);
  (d) a plain non-operator Service still emits NO cluster RBAC (unchanged)
      — it gets the namespaced Role and no ClusterRole/ClusterRoleBinding.
"""

import forge

# ── The operator, authored as an agnostic Service subtype ───────────
# `schema WorkspaceController(forge.Service)` — an operator is JUST a
# Service with a k8s.operator tail. Nothing here is operator-specific
# except the `k8s.operator` block.
schema WorkspaceController(forge.Service):
    name = "workspace-controller"
    image = "control-plane"
    command = ["./control-plane", "workspace"]
    # The RPC listener the legacy Operator could not express. This single
    # `ports` fact generates the containerPort AND the :9191 Service object
    # that admin-server dials — no hand-written Service, no triplicated
    # literal.
    ports = [9191]
    env = {
        # Leader election is a plain env default (operatorkit.Run reads it
        # at runtime) — NOT a first-class field.
        "LEADER_ELECTION_ID" = {value = "workspace-controller"}
    }
    k8s = forge.K8sOverrides {
        operator = forge.OperatorSpec {
            # Cluster-scoped PolicyRule dicts — a real subset of
            # control-plane's 12-rule set (the CRD reconcile rule + the
            # leader-election leases rule).
            cluster_rbac = [
                {
                    apiGroups = ["reliant.dev"]
                    resources = ["workspaces", "workspaces/status", "workspaces/finalizers"]
                    verbs = ["get", "list", "watch", "create", "update", "patch", "delete"]
                }
                {
                    apiGroups = ["coordination.k8s.io"]
                    resources = ["leases"]
                    verbs = ["get", "list", "watch", "create", "update", "patch", "delete"]
                }
            ]
            crds = ["Workspace"]
        }
    }

_controller = WorkspaceController {}

# ── A plain, NON-operator Service — the (d) control ─────────────────
_admin = forge.Service {
    name = "admin-server"
    image = "control-plane"
    command = ["./control-plane", "serve"]
    ports = [8080]
}

# ── Env-wide k8s coordinates + render (adapter runs INTERNALLY) ──────
_prod = forge.ClusterTarget {
    cluster = "prod-cluster"
    namespace = "control-plane-prod"
    registry = "ghcr.io/reliant-labs"
}

_bundle = forge.Bundle {
    cluster_target = _prod
    workloads = [_controller, _admin]
}
manifests = forge.render_manifests(_bundle, "latest", {}, False)

_deploys = {m.metadata.name: m for m in manifests if m.kind == "Deployment"}
_svcs = {m.metadata.name: m for m in manifests if m.kind == "Service"}
_cluster_roles = {m.metadata.name: m for m in manifests if m.kind == "ClusterRole"}
_cluster_role_bindings = {m.metadata.name: m for m in manifests if m.kind == "ClusterRoleBinding"}
_roles = {m.metadata.name: m for m in manifests if m.kind == "Role"}

# ── (a) the Deployment renders ──────────────────────────────────────
assert_deploy_exists = "workspace-controller" in _deploys
_dep = _deploys["workspace-controller"]
assert_deploy_image = _dep.spec.template.spec.containers[0].image == "ghcr.io/reliant-labs/control-plane:latest"
assert_deploy_command = _dep.spec.template.spec.containers[0].command == ["./control-plane", "workspace"]
# The pod binds the operator's own ServiceAccount (emitted by cluster RBAC).
assert_deploy_binds_sa = _dep.spec.template.spec.serviceAccountName == "workspace-controller"
# containerPort comes from `ports`.
assert_container_port = _dep.spec.template.spec.containers[0].ports[0].containerPort == 9191

# ── (b) THE WIN: a :9191 Service is emitted FROM `ports` ────────────
# The legacy Operator had no `ports`, so this Service had to be hand-written
# and the port literal triplicated. Now it falls out of the one core fact.
assert_svc_exists = "workspace-controller" in _svcs
_svc_ports = _svcs["workspace-controller"].spec.ports
assert_svc_port_9191 = _svc_ports[0].port == 9191 and _svc_ports[0].targetPort == 9191

# ── (c) cluster RBAC: ClusterRole + ClusterRoleBinding, right name ──
# The name MUST be `workspace-controller-clusterrole` — control-plane's
# admin-server rebind (lib/rbac.k) binds that exact name to the admin SA.
assert_clusterrole_exists = "workspace-controller-clusterrole" in _cluster_roles
_cr = _cluster_roles["workspace-controller-clusterrole"]
# Rules = forge's default config-read rule + the two supplied rules.
assert_clusterrole_rule_count = len(_cr.rules) == 3
assert_clusterrole_default_first = _cr.rules[0] == {
    apiGroups = [""]
    resources = ["configmaps", "secrets"]
    verbs = ["get", "list", "watch"]
}
assert_clusterrole_crd_rule = _cr.rules[1].apiGroups == ["reliant.dev"] and _cr.rules[1].resources == ["workspaces", "workspaces/status", "workspaces/finalizers"]
assert_clusterrole_leases_rule = _cr.rules[2].apiGroups == ["coordination.k8s.io"] and _cr.rules[2].resources == ["leases"]

# ClusterRoleBinding binds that ClusterRole to the operator's SA.
assert_crb_exists = "workspace-controller-clusterrolebinding" in _cluster_role_bindings
_crb = _cluster_role_bindings["workspace-controller-clusterrolebinding"]
assert_crb_roleref = _crb.roleRef.kind == "ClusterRole" and _crb.roleRef.name == "workspace-controller-clusterrole"
assert_crb_subject = _crb.subjects[0] == {kind = "ServiceAccount", name = "workspace-controller", namespace = "control-plane-prod"}

# The operator gets cluster RBAC INSTEAD OF a namespaced Role — the two
# tiers are mutually exclusive, so no namespaced Role is emitted for it.
assert_operator_no_namespaced_role = "workspace-controller-role" not in _roles

# ── (d) a plain Service still emits NO cluster RBAC (unchanged) ──────
assert_admin_deploy_exists = "admin-server" in _deploys
# NO ClusterRole / ClusterRoleBinding for the plain service.
assert_admin_no_clusterrole = "admin-server-clusterrole" not in _cluster_roles
assert_admin_no_crb = "admin-server-clusterrolebinding" not in _cluster_role_bindings
# It DOES get the namespaced Role (byte-identical to before).
assert_admin_has_namespaced_role = "admin-server-role" in _roles
assert_admin_role_default_only = _roles["admin-server-role"].rules == [{
    apiGroups = [""]
    resources = ["configmaps", "secrets"]
    verbs = ["get", "list", "watch"]
}]

# ── crds bridge: the deploy-as-data JSON carries the CRD name list ──
# render() (not render_manifests) exposes the service-level JSON contract;
# operator_crds is the codegen (AddToScheme) bridge — populated for the
# operator, null for the plain service.
_data = forge.render(_bundle)
_svc_data = {s.name: s for s in _data.services}
assert_crds_in_json = _svc_data["workspace-controller"].operator_crds == ["Workspace"]
assert_no_crds_plain = _svc_data["admin-server"].operator_crds == None
