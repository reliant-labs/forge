"""
Positive test — `forge.flux_chart(egress = ...)` replaces the chart's open
egress with per-controller allow-lists.

NetworkPolicy allows are ADDITIVE: a policy added beside `allow-egress` with
`egress: [{}]` is cancelled by it. So the parameter must remove the `{}` rule,
not add to it. Pins: the default is untouched (`[{}]`), a declared egress leaves
NO `{}` anywhere, each controller gets its own policy selected by pod label, and
the ingress invariant (flux-system pods only) is unchanged.
"""

import forge

_dns = {"to" = [{podSelector.matchLabels = {"k8s-app" = "kube-dns"}}], ports = [{port = 53, protocol = "UDP"}]}
_src = {"to" = [{podSelector.matchLabels = {app = "source-controller"}}], ports = [{port = 9090, protocol = "TCP"}]}
_pub = {"to" = [{ipBlock = {cidr = "0.0.0.0/0", except = ["10.0.0.0/8"]}}], ports = [{port = 443, protocol = "TCP"}]}

_default = forge.flux_chart()
_declared = forge.flux_chart(egress = {
    kustomize = [_dns, _src]
    source = [_dns, _pub]
    flux_check = [_dns]
})
_by = lambda c: any, kind: str, name: str -> any { [o for o in c.values.extraObjects if o.kind == kind and o.metadata.name == name][0] }
_names = lambda c: any -> [str] { [o.metadata.name for o in c.values.extraObjects if o.kind == "NetworkPolicy"] }

# ── Default: byte-for-byte what forge has always rendered ──────────
assert_default_policies = _names(_default) == ["allow-egress"]
assert_default_open = _by(_default, "NetworkPolicy", "allow-egress").spec.egress == [{}]
assert_default_ingress = _by(_default, "NetworkPolicy", "allow-egress").spec.ingress == [{"from" = [{podSelector = {}}]}]
assert_default_one_deployment_arg_set = len([a for a in _default.values.kustomizeController.container.additionalArgs if "watch-label-selector" in a]) == 0

# ── Declared: exactly the given rules, and no `{}` ─────────────────
assert_declared_policies = _names(_declared) == ["kustomize-controller-egress", "source-controller-egress", "flux-check-egress", "allow-egress"]
_ke = _by(_declared, "NetworkPolicy", "kustomize-controller-egress")
_se = _by(_declared, "NetworkPolicy", "source-controller-egress")
_fe = _by(_declared, "NetworkPolicy", "flux-check-egress")
assert_kustomize_exact = _ke.spec.egress == [_dns, _src]
assert_source_exact = _se.spec.egress == [_dns, _pub]
assert_check_exact = _fe.spec.egress == [_dns]
assert_selected_by_app = _ke.spec.podSelector.matchLabels == {app = "kustomize-controller"} and _se.spec.podSelector.matchLabels == {app = "source-controller"}
assert_check_selected_by_job = _fe.spec.podSelector.matchLabels == {"job-name" = "flux-flux-check"}
assert_egress_only = _ke.spec.policyTypes == ["Egress"] and _se.spec.policyTypes == ["Egress"]
_ae = _by(_declared, "NetworkPolicy", "allow-egress")
# allow-egress still selects every pod for Egress, now with NO rule: default deny.
assert_allow_egress_denies = _ae.spec.podSelector == {} and _ae.spec.policyTypes == ["Ingress", "Egress"] and "egress" not in _ae.spec
# THE INVARIANT survives: ingress to every flux-system pod only from flux-system pods.
assert_ingress_invariant = _ae.spec.ingress == [{"from" = [{podSelector = {}}]}] and "namespaceSelector" not in str(_ae.spec.ingress)
assert_no_open_rule_anywhere = len([o for o in _declared.values.extraObjects if o.kind == "NetworkPolicy" and ({} in (o.spec.egress or []))]) == 0
# The allows precede the deny in the rendered order, so a re-apply over a live open policy never has a window with neither.
assert_allows_before_deny = _names(_declared)[-1] == "allow-egress"
# An explicit [{}] is the way to opt one controller back to open.
_opt = forge.flux_chart(egress = {kustomize = [_dns], source = [{}], flux_check = [{}]})
assert_explicit_open_is_allowed = _by(_opt, "NetworkPolicy", "source-controller-egress").spec.egress == [{}]
