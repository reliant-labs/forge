"""
Positive test — the `service_account` override and the generated RBAC name ONE
identity.

`K8sOverrides.service_account` binds an EXISTING out-of-band ServiceAccount
(the documented case: an SA carrying an `iam.gke.io/gcp-service-account`
workload-identity annotation so a Cloud SQL proxy sidecar can authenticate).
lib/services.k honored it for the POD, but render.k passed `s.name` to the RBAC
builders, so forge emitted a ServiceAccount + Role + RoleBinding trio named
after the SERVICE while the pod ran as a DIFFERENT SA. The grant landed on an
identity with no pods:

    Deployment  daemon-gateway              -> serviceAccountName: reliant-cloudsql
    RoleBinding daemon-gateway-rolebinding  -> subject ServiceAccount daemon-gateway

Measured in control-plane's prod render, for three workloads; it is what `forge
doctor`'s Deploy SA Binding reports as "rendered ServiceAccount(s) are never
bound to a pod".

THE INVARIANT, and it is deliberately stated as a RELATION rather than as two
literals: the RoleBinding's subject == the pod's serviceAccountName. Asserting
the overridden case alone would pass for an implementation that ALWAYS used the
override, so the un-overridden service below is the positive control that pins
the other half. Both halves are load-bearing.

Also pinned here:
  * forge does NOT emit a ServiceAccount object for an overridden SA — it does
    not own an identity it was told already exists, and several workloads may
    legitimately share one (control-plane points three at `reliant-cloudsql`),
    which would otherwise render the same SA repeatedly WITHOUT the
    workload-identity annotation the real one carries.
  * imagePullSecrets survive the override by riding the POD, since there is no
    generated SA to carry them. Losing them surfaces as ImagePullBackOff on a
    private registry, arbitrarily far from the SA field that caused it.
  * The ROLE keeps the WORKLOAD's name (not the SA's), so two services sharing
    one SA keep their own distinct rule sets instead of clobbering each other.
  * The operator (cluster-RBAC) tier behaves identically.
"""

import forge

_target = forge.ClusterTarget {
    cluster = "k3d-example"
    namespace = "example-dev"
    registry = "ghcr.io/reliant/private"
    image_pull_secrets = ["ghcr-creds"]
}

_bundle = forge.Bundle {
    cluster_target = _target
    services = [
        # OVERRIDDEN: binds an existing out-of-band SA, and also declares
        # extra RBAC — the combination must keep BOTH (an override is not a
        # reason to silently drop a declared rule).
        forge.RenderedWorkload {
            name = "gateway"
            image = "gw-img"
            deploy = _target.deploy | {ports = [8080]}
            service_account = "reliant-cloudsql"
            namespaced_rbac = [
                {apiGroups = ["coordination.k8s.io"], resources = ["leases"], verbs = ["get", "update"]}
            ]
        }
        # A SECOND workload sharing the SAME override — control-plane's real
        # shape (three workloads, one `reliant-cloudsql` SA). Pins that forge
        # does not emit the shared SA twice, and that each keeps its own Role.
        forge.RenderedWorkload {
            name = "worker"
            image = "worker-img"
            deploy = _target.deploy | {ports = [8080]}
            service_account = "reliant-cloudsql"
        }
        # POSITIVE CONTROL: no override. The subject must follow THIS pod to
        # its own generated SA. Without this case, "always use the override"
        # would pass every assertion above.
        forge.RenderedWorkload {
            name = "plain"
            image = "plain-img"
            deploy = _target.deploy | {ports = [8080]}
        }
        # Operator tier (cluster RBAC) carrying the same override.
        forge.RenderedWorkload {
            name = "controller"
            image = "controller-img"
            deploy = _target.deploy | {ports = [8080]}
            service_account = "reliant-cloudsql"
            operator_cluster_rbac = [
                {apiGroups = [""], resources = ["pods"], verbs = ["get", "list", "watch"]}
            ]
        }
    ]
}

_manifests = forge.render_manifests(_bundle, "abc1234", {}, False)

_deploy = lambda n: str -> any {
    [m for m in _manifests if m.kind == "Deployment" and m.metadata.name == n][0]
}
_binding = lambda n: str -> any {
    [m for m in _manifests if m.kind == "RoleBinding" and m.metadata.name == "${n}-rolebinding"][0]
}

_gateway_pod = _deploy("gateway").spec.template.spec
_worker_pod = _deploy("worker").spec.template.spec
_plain_pod = _deploy("plain").spec.template.spec
_controller_pod = _deploy("controller").spec.template.spec

_cluster_binding = [
    m for m in _manifests
    if m.kind == "ClusterRoleBinding" and m.metadata.name == "controller-example-dev-clusterrolebinding"
][0]

_sa_names = [m.metadata.name for m in _manifests if m.kind == "ServiceAccount"]

# ── THE INVARIANT: the binding's subject follows the pod ───────────────
#
# Stated as a relation between two rendered values, so neither an
# "always override" nor an "always s.name" implementation can satisfy it.

# Overridden: subject tracks the override the pod binds.
assert_gateway_subject_follows_pod = \
    _binding("gateway").subjects[0].name == _gateway_pod.serviceAccountName
assert_worker_subject_follows_pod = \
    _binding("worker").subjects[0].name == _worker_pod.serviceAccountName
# POSITIVE CONTROL — un-overridden: subject tracks the generated SA.
assert_plain_subject_follows_pod = \
    _binding("plain").subjects[0].name == _plain_pod.serviceAccountName
# Operator tier, same relation at cluster scope.
assert_controller_subject_follows_pod = \
    _cluster_binding.subjects[0].name == _controller_pod.serviceAccountName

# The relation holds against DIFFERENT identities on each side — proof the
# two cases are genuinely distinguished rather than coincidentally equal.
assert_gateway_runs_as_override = _gateway_pod.serviceAccountName == "reliant-cloudsql"
assert_plain_runs_as_own_sa = _plain_pod.serviceAccountName == "plain"

# ── forge does not emit an SA it was told already exists ───────────────

# `plain` is forge's own identity, so forge generates it.
assert_plain_sa_generated = "plain" in _sa_names
# The override is NOT generated — not once, and not once per sharing workload.
assert_override_sa_not_generated = "reliant-cloudsql" not in _sa_names
assert_overridden_names_not_generated = "gateway" not in _sa_names and "worker" not in _sa_names
assert_operator_override_sa_not_generated = "controller" not in _sa_names

# ── the Role keeps the WORKLOAD's name, and its declared rules ─────────

_gateway_role = [m for m in _manifests if m.kind == "Role" and m.metadata.name == "gateway-role"][0]
_worker_role = [m for m in _manifests if m.kind == "Role" and m.metadata.name == "worker-role"][0]

# Two workloads share one SA and still get their own distinct rule sets —
# naming the Role after the SA would have made the second clobber the first.
assert_gateway_keeps_declared_rule = _gateway_role.rules[1].resources == ["leases"]
assert_worker_role_default_only = len(_worker_role.rules) == 1
assert_roleref_names_the_workload = _binding("gateway").roleRef.name == "gateway-role"

# ── imagePullSecrets survive the override, on the pod ──────────────────

# No generated SA to carry them, so they ride the pod spec instead.
assert_override_pull_on_pod = _gateway_pod.imagePullSecrets == [{name = "ghcr-creds"}]
# The DEFAULT path is unchanged: still on the SA, still absent from the pod.
assert_plain_pull_not_on_pod = _plain_pod.imagePullSecrets == Undefined
_plain_sa = [m for m in _manifests if m.kind == "ServiceAccount" and m.metadata.name == "plain"][0]
assert_plain_pull_on_sa = _plain_sa.imagePullSecrets == [{name = "ghcr-creds"}]
