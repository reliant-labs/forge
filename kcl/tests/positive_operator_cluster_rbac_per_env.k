"""Two envs sharing ONE cluster must not write the same cluster-scoped RBAC.

positive_operator_service.k pins the NAME an operator's ClusterRole and
ClusterRoleBinding get. This fixture pins the PROPERTY that name exists to
serve, which is the thing that actually broke twice in control-plane's k3d
cluster: render the SAME operator into TWO namespaces and the two renders must
not collide on any cluster-scoped address.

WHY BOTH FIXTURES. A literal-name assertion is satisfied by any naming scheme
someone writes down, including a scheme that reintroduces the collision under a
different spelling (suffix the CLUSTER instead of the namespace, say, or hash
the two together and truncate). This one cannot be satisfied that way: it
compares two renders and fails on any shared cluster-scoped name, so it keeps
holding through a rename that the literal assertions would have to be edited to
follow.

THE DEFECT IT GUARDS. A ClusterRole and a ClusterRoleBinding have exactly one
name per cluster, while the binding's `subjects[0].namespace` is the deploying
env's namespace. Name them after the operator alone and every env writes the
same two objects — whichever deploys last silently repoints the binding at its
own namespace, and the loser's controller keeps its Deployment, its pods and
its ServiceAccount while losing every cluster-scoped grant. Observed in
control-plane: e2e broken while the live binding's subject pointed at
control-plane-dev, and the workspace-proxy variant stalled a resolver informer
until Envoy returned 504. Nothing in a single-env render can catch it, which is
why this fixture renders twice.

Asserts:
  (a) the two envs share NO cluster-scoped object name (ClusterRole,
      ClusterRoleBinding) — the property;
  (b) each env's binding still roleRefs ITS OWN ClusterRole, so the grant
      resolves rather than dangling at a name nothing renders;
  (c) each env's binding subject names ITS OWN namespace;
  (d) the namespaced ServiceAccount is deliberately NOT suffixed — it is
      already unique per (namespace, name), and workloads reference it bare.
"""

import forge

schema WorkspaceController(forge.Service):
    name = "workspace-controller"
    image = "control-plane"
    command = ["./control-plane", "workspace"]
    k8s = forge.K8sOverrides {
        operator = forge.OperatorSpec {
            cluster_rbac = [
                {
                    apiGroups = ["reliant.dev"]
                    resources = ["workspaces"]
                    verbs = ["get", "list", "watch"]
                }
            ]
            crds = ["Workspace"]
        }
    }

_controller = WorkspaceController {}

# The two envs that share k3d-control-plane in the real project. Same cluster,
# same operator, DIFFERENT namespaces — the exact shape that collided.
_dev = forge.ClusterTarget {
    cluster = "k3d-control-plane"
    namespace = "control-plane-dev"
    registry = "ghcr.io/reliant-labs"
}

_e2e = forge.ClusterTarget {
    cluster = "k3d-control-plane"
    namespace = "control-plane-e2e"
    registry = "ghcr.io/reliant-labs"
}

_dev_manifests = forge.render_manifests(forge.Bundle {
    cluster_target = _dev
    workloads = [_controller]
}, "latest", {}, False)

_e2e_manifests = forge.render_manifests(forge.Bundle {
    cluster_target = _e2e
    workloads = [_controller]
}, "latest", {}, False)

_CLUSTER_SCOPED = ["ClusterRole", "ClusterRoleBinding"]

_dev_cluster_names = [m.metadata.name for m in _dev_manifests if m.kind in _CLUSTER_SCOPED]
_e2e_cluster_names = [m.metadata.name for m in _e2e_manifests if m.kind in _CLUSTER_SCOPED]

# ── (a) THE PROPERTY: no shared cluster-scoped address ───────────────
# Guard first that there is anything to compare — an empty intersection is
# trivially true, so a render that emitted NO cluster RBAC would pass (a)
# while proving nothing.
assert_dev_emits_cluster_rbac = len(_dev_cluster_names) == 2
assert_e2e_emits_cluster_rbac = len(_e2e_cluster_names) == 2
_shared = [n for n in _dev_cluster_names if n in _e2e_cluster_names]
assert_no_shared_cluster_scoped_names = len(_shared) == 0

# ── (b) each binding resolves to a role THIS env renders ─────────────
# A suffixed binding whose roleRef still named the old shared role would
# satisfy (a) and grant nothing: the API server accepts a ClusterRoleBinding
# pointing at a missing ClusterRole without complaint.
_dev_roles = [m.metadata.name for m in _dev_manifests if m.kind == "ClusterRole"]
_e2e_roles = [m.metadata.name for m in _e2e_manifests if m.kind == "ClusterRole"]
_dev_crb = [m for m in _dev_manifests if m.kind == "ClusterRoleBinding"][0]
_e2e_crb = [m for m in _e2e_manifests if m.kind == "ClusterRoleBinding"][0]
assert_dev_roleref_resolves = _dev_crb.roleRef.name in _dev_roles
assert_e2e_roleref_resolves = _e2e_crb.roleRef.name in _e2e_roles

# ── (c) each binding's subject names ITS OWN namespace ──────────────
# This is the field that made the collision harmful rather than idempotent.
assert_dev_subject_ns = _dev_crb.subjects[0].namespace == "control-plane-dev"
assert_e2e_subject_ns = _e2e_crb.subjects[0].namespace == "control-plane-e2e"

# ── (d) the ServiceAccount is NOT suffixed ───────────────────────────
# Namespaced objects need no help being unique, and suffixing the SA would
# break every `serviceAccountName` that references it by bare name.
_dev_sas = [m.metadata.name for m in _dev_manifests if m.kind == "ServiceAccount"]
_e2e_sas = [m.metadata.name for m in _e2e_manifests if m.kind == "ServiceAccount"]
assert_dev_sa_bare = "workspace-controller" in _dev_sas
assert_e2e_sa_bare = "workspace-controller" in _e2e_sas
_dev_deploy = [m for m in _dev_manifests if m.kind == "Deployment"][0]
assert_dev_pod_binds_bare_sa = _dev_deploy.spec.template.spec.serviceAccountName == "workspace-controller"
