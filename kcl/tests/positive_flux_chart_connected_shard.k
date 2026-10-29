"""
Positive test — `forge.flux_chart(connected_shard = True)` renders a SECOND
kustomize-controller for connected clusters, and "apply as a service account" is
a hard invariant on it, not a convention.

  * the shard has NO --default-service-account, no workload identity, and a
    ServiceAccount with no impersonate or serviceaccounts access anywhere;
  * the main controller keeps --default-service-account and the tenant
    impersonation on the hub, and watches `notin (connected)`;
  * a ValidatingAdmissionPolicy (failurePolicy Fail) refuses a shard
    Kustomization with serviceAccountName / without kubeConfig.secretRef, and a
    kubeConfig Kustomization that is not on the shard (behaviour proven against
    a real apiserver by TestFluxConnectedShardAdmission).
"""

import forge

_dns = {"to" = [{podSelector.matchLabels = {"k8s-app" = "kube-dns"}}], ports = [{port = 53, protocol = "UDP"}]}
_pub = {"to" = [{ipBlock = {cidr = "0.0.0.0/0", except = ["10.0.0.0/8"]}}], ports = [{port = 443, protocol = "TCP"}]}
_c = forge.flux_chart(connected_shard = True, egress = {
    kustomize = [_dns]
    source = [_dns]
    kustomize_connected = [_dns, _pub]
    flux_check = [_dns]
})
_x = _c.values.extraObjects
_of = lambda kind: str -> [any] { [o for o in _x if o.kind == kind] }
_by = lambda kind: str, name: str -> any { [o for o in _x if o.kind == kind and o.metadata.name == name][0] }
_shard = _by("Deployment", "kustomize-controller-connected")
_args = _shard.spec.template.spec.containers[0].args
_main = _c.values.kustomizeController.container.additionalArgs

# ── Two controllers, selected by the sharding label ────────────────
assert_main_selector = "--watch-label-selector=sharding.fluxcd.io/key notin (connected)" in _main
assert_shard_selector = "--watch-label-selector=sharding.fluxcd.io/key=connected" in _args
assert_shard_is_deployment_in_flux_system = _shard.metadata.namespace == "flux-system" and _shard.spec.selector.matchLabels == {app = "kustomize-controller-connected"}
assert_shard_image_is_pinned = _shard.spec.template.spec.containers[0].image == "ghcr.io/fluxcd/kustomize-controller:v1.9.5"
assert_shard_hermetic = "--no-cross-namespace-refs=true" in _args and "--no-remote-bases=true" in _args
assert_shard_leader_election = "--enable-leader-election" in _args

# ── The ban: no service account on the shard ───────────────────────
assert_shard_has_no_default_sa_flag = len([a for a in _args if "default-service-account" in a]) == 0
assert_main_keeps_default_sa = _c.values.multitenancy.defaultServiceAccount == "reliant-deploy-tenant" and _c.values.multitenancy.enabled == True
# No workload identity on the shard's pod or its ServiceAccount.
_sa = _by("ServiceAccount", "kustomize-controller-connected")
assert_shard_sa_no_annotations = "annotations" not in _sa.metadata
assert_shard_pod_no_wi = "iam.gke.io/gcp-service-account" not in str(_shard) and "nodeSelector" not in _shard.spec.template.spec
# Every ClusterRole/Role bound to the shard, found by following the bindings.
_bound_roles = lambda -> [str] {
    [b.roleRef.name for b in _x if b.kind in ["ClusterRoleBinding", "RoleBinding"] and "kustomize-controller-connected" in [s.name for s in b.subjects]]
}
_rules = [r for o in _x if o.kind in ["ClusterRole", "Role"] and o.metadata.name in _bound_roles() for r in o.rules]
assert_shard_has_roles = len(_rules) > 0 and "flux-connected-controller" in _bound_roles()
assert_no_impersonate = len([r for r in _rules if "impersonate" in r.verbs]) == 0
assert_no_wildcard_verbs = len([r for r in _rules if "*" in r.verbs or "*" in r.resources]) == 0
assert_no_serviceaccounts_access = len([r for r in _rules if "serviceaccounts" in r.resources or "serviceaccounts/token" in r.resources]) == 0
# Never granted the tenant reconciler (which carries impersonate) or the source role.
assert_never_bound_to_tenant_roles = len([n for n in _bound_roles() if n in [forge.FLUX_RECONCILER_ROLE, forge.FLUX_SOURCE_ROLE, "crd-controller"]]) == 0
# Secret reads are per-namespace: the cluster-wide role grants none.
assert_no_clusterwide_secret_read = len([r for o in _x if o.kind == "ClusterRole" and o.metadata.name == "flux-connected-controller" for r in o.rules if "secrets" in r.resources or "configmaps" in r.resources]) == 0
assert_secret_read_is_namespaced_role = _by("ClusterRole", "flux-connected-reconciler").rules == [{apiGroups = [""], resources = ["secrets"], verbs = ["get", "list", "watch"]}]
# The main controller's impersonate grant is separate and unchanged.
_imp = [r for r in _by("ClusterRole", "flux-tenant-reconciler").rules if "impersonate" in r.verbs][0]
assert_main_impersonation_unchanged = _imp.resourceNames == ["reliant-deploy-tenant"]
assert_main_binding_subject_unchanged = len([b for b in forge.flux_namespace_bindings("flux-acme") if "connected" in b.metadata.name]) == 0

# ── Per-namespace binding of the shard ─────────────────────────────
_nb = forge.flux_namespace_bindings("flux-acme", connected_shard = True)
assert_ns_bindings = sorted([b.roleRef.name for b in _nb]) == ["flux-connected-reconciler", "flux-tenant-reconciler", "flux-tenant-source"]
_cb = [b for b in _nb if b.roleRef.name == "flux-connected-reconciler"][0]
assert_ns_binding_subject = _cb.subjects == [{kind = "ServiceAccount", name = "kustomize-controller-connected", namespace = "flux-system"}] and _cb.metadata.namespace == "flux-acme"
assert_tenant_binding_not_given_to_shard = len([b for b in _nb if b.roleRef.name == "flux-tenant-reconciler" and b.subjects[0].name != "kustomize-controller"]) == 0

# ── Egress per controller ──────────────────────────────────────────
assert_connected_policy = _by("NetworkPolicy", "kustomize-controller-connected-egress").spec.egress == [_dns, _pub]
assert_connected_policy_selector = _by("NetworkPolicy", "kustomize-controller-connected-egress").spec.podSelector.matchLabels == {app = "kustomize-controller-connected"}
assert_main_has_no_internet = "ipBlock" not in str(_by("NetworkPolicy", "kustomize-controller-egress").spec.egress)

# ── Admission: failurePolicy Fail, Deny, both directions ───────────
_vap = _by("ValidatingAdmissionPolicy", "flux-connected-shard-no-serviceaccount")
assert_vap_fail_closed = _vap.spec.failurePolicy == "Fail"
assert_vap_binding_denies = _by("ValidatingAdmissionPolicyBinding", "flux-connected-shard-no-serviceaccount").spec.validationActions == ["Deny"]
assert_vap_targets_kustomizations = _vap.spec.matchConstraints.resourceRules[0].resources == ["kustomizations"] and _vap.spec.matchConstraints.resourceRules[0].operations == ["CREATE", "UPDATE"]
assert_vap_four_rules = len(_vap.spec.validations) == 4

# ── Only the connected controller is placed/looked-after the same way ──
assert_leader_election_binding_includes_shard = "kustomize-controller-connected" in [s.name for s in _by("RoleBinding", "flux-leader-election").subjects]

# And the lot passes the schema and renders.
_out = forge.render(forge.Bundle {project = "fixture", env = "dev", helm_charts = [_c]})
assert_renders = len(_out.helm_charts) == 1

# ── Without the flag nothing changes ───────────────────────────────
_plain = forge.flux_chart()
assert_plain_has_no_shard = len([o for o in _plain.values.extraObjects if "connected" in o.metadata.name]) == 0
assert_plain_main_has_no_selector = len([a for a in _plain.values.kustomizeController.container.additionalArgs if "watch-label-selector" in a]) == 0
