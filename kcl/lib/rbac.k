"""
Forge KCL — RBAC builders.

Two flavors:

  * Namespaced (Role + RoleBinding) — every Service/CronJob gets a
    default Role for reading its config (configmaps/secrets get/list/
    watch).
  * Cluster-scoped (ClusterRole + ClusterRoleBinding) — every Operator
    gets one. Default rules + caller-supplied rules from
    `operator.cluster_rbac.rules`.

Both forms also emit a ServiceAccount.

Lambdas take `any` for the entity / RBAC arg by convention — see the
package-identity note in `lib/services.k` for why.
"""

import .labels as labels_lib

# The shared forge-managed label set — see lib/labels.k. `part_of` is the
# PROJECT these objects belong to, so a CD pipeline can select every object
# of one release with `-l app.kubernetes.io/part-of=<project>`.
_managed_labels = labels_lib.managed_labels

# The default RBAC rules a workload that DOES call the Kubernetes API
# gets — read the projected ConfigMap + Secret for its per-env config.
#
# Only cluster-RBAC callers (operators) get these. An ordinary
# Server/Worker/Cron receives its config through the container `env`
# block, projected at render time; it never opens a client-go
# connection, so granting it `get/list/watch` on every Secret in the
# namespace buys nothing and hands an RCE a credential dump. See
# render_service_account.
_default_rules: [{str: any}] = [
    {
        apiGroups = [""]
        resources = ["configmaps", "secrets"]
        verbs = ["get", "list", "watch"]
    }
]

# A ServiceAccount with NO Role and NO token, for a workload that does
# not talk to the Kubernetes API — which is every forge Server, Worker,
# Binary and Cron: their configuration is projected into the container
# env at render time.
#
# The identity still matters. Without a bound ServiceAccount the pod
# runs as the namespace's `default` SA, inherits whatever anyone has
# granted that account, and is indistinguishable from every other
# workload in audit logs and in any policy that selects on identity.
# `automountServiceAccountToken = false` completes it: an API token
# mounted into a process that never calls the API is pure blast radius.
#
# The previous shape emitted a ServiceAccount, a Role granting
# get/list/watch on ALL secrets with no resourceNames, and a
# RoleBinding — and then never set serviceAccountName on any pod. It
# read like scoped RBAC, granted more than any forge workload needs,
# and bound to nothing. Manifests that look like scoped RBAC and are
# not are worse than none.
render_service_account = lambda name: str, namespace: str, part_of: str = "", pull_secrets: [str] = [] -> [any] {
    [{
        apiVersion = "v1"
        kind = "ServiceAccount"
        metadata = {name = name, namespace = namespace, labels = _managed_labels(name, part_of)}
        automountServiceAccountToken = False
        if pull_secrets:
            imagePullSecrets = [{name = s} for s in pull_secrets]
    }]
}

# `extra_rules` are PolicyRule dicts appended to the default config-read
# Role (e.g. coordination.k8s.io/leases for a Service running a
# controller-runtime manager). `pull_secrets` are names of EXISTING
# image-pull Secrets; when set they're attached to the generated
# ServiceAccount so every pod/Job that binds it can pull from a private
# registry. Both default to empty — callers that don't need them keep
# the original two-arg behavior and emit identical manifests.
#
# `service_account` is the workload's `K8sOverrides.service_account`: an
# EXISTING out-of-band SA (typically carrying a cloud IAM
# workload-identity annotation) that the POD binds instead of the
# forge-generated one. Empty (the common case) => byte-identical to
# before: forge generates the SA and binds the Role to it.
#
# When it IS set, two things change together, and they have to change
# together or the grant is inert:
#
#   * The RoleBinding's SUBJECT becomes the override. The Role exists to
#     grant permission to the identity that actually runs; binding it to
#     a name no pod uses produces manifests that read as scoped RBAC and
#     grant nothing. That was the defect this argument fixes — the pod
#     bound `reliant-cloudsql` while `daemon-gateway-rolebinding` named
#     `daemon-gateway`, an identity with no pods, which is what `forge
#     doctor`'s Deploy SA Binding reported.
#   * The ServiceAccount object is NOT emitted. forge does not own an SA
#     it was explicitly told already exists, and emitting one is worse
#     than redundant: several workloads may legitimately share ONE
#     override (control-plane points three at `reliant-cloudsql`), so
#     forge would render the same SA repeatedly, each copy WITHOUT the
#     workload-identity annotation the real one carries — an apply would
#     strip it and break the auth the override existed to provide.
#
# The ROLE keeps the workload's name rather than the SA's, deliberately.
# The rule set belongs to the workload, not to the identity: two services
# sharing one SA legitimately declare different rules, and naming the
# Role after the SA would make the second render clobber the first's
# grants — the same last-writer-wins failure render_cluster_rbac
# documents below at cluster scope. Sharing an SA therefore yields
# several Roles and several RoleBindings with one subject, which is
# redundant but correct: RBAC grants union.
#
# The imagePullSecrets consequence is handled at the POD, in
# lib/services.k — see the `_pull_secrets_on_pod` note there.
render_namespaced_rbac = lambda name: str, namespace: str, extra_rules: [{str: any}] = [], pull_secrets: [str] = [], part_of: str = "", service_account: str = "" -> [any] {
    _labels = _managed_labels(name, part_of)
    # The identity the Role is granted TO: the override when the pod
    # binds one, else the SA forge generates below.
    _subject = service_account if service_account else name
    ([] if service_account else [
        {
            apiVersion = "v1"
            kind = "ServiceAccount"
            metadata = {name = name, namespace = namespace, labels = _labels}
            if pull_secrets:
                imagePullSecrets = [{name = s} for s in pull_secrets]
        }
    ]) + [
        {
            apiVersion = "rbac.authorization.k8s.io/v1"
            kind = "Role"
            metadata = {
                name = "${name}-role"
                namespace = namespace
                labels = _labels
            }
            rules = _default_rules + extra_rules
        }
        {
            apiVersion = "rbac.authorization.k8s.io/v1"
            kind = "RoleBinding"
            metadata = {
                name = "${name}-rolebinding"
                namespace = namespace
                labels = _labels
            }
            subjects = [
                {kind = "ServiceAccount", name = _subject, namespace = namespace}
            ]
            roleRef = {
                apiGroup = "rbac.authorization.k8s.io"
                kind = "Role"
                name = "${name}-role"
            }
        }
    ]
}

# `pull_secrets` mirror render_namespaced_rbac — names of EXISTING
# image-pull Secrets attached to the generated ServiceAccount so the
# operator's pods (and any Jobs it spawns under this SA) can pull from a
# private registry. Defaults to empty for backward-compatible callers.
#
# THE NAMES CARRY THE NAMESPACE, and that is a correctness requirement rather
# than a cosmetic convention. A ClusterRole and a ClusterRoleBinding are
# cluster-scoped: they have exactly ONE name per cluster. The BINDING's
# `subjects[0].namespace`, however, is the deploying ENV's namespace — so with
# an env-invariant name every env in a shared cluster writes the same two
# objects, and whichever deploys last silently repoints the binding at its own
# namespace. The loser's operator SA keeps its Deployment, its pods and its
# ServiceAccount, and loses every cluster-scoped grant: leader-election leases
# and CR watches start returning `is forbidden` on each tick.
#
# It is a genuinely invisible failure. Each env renders correctly in isolation,
# every manifest is valid, and `kubectl apply` overwrites a same-named object
# without a word — so it surfaces only downstream as one env's controller
# inexplicably losing permissions it plainly declares. Measured twice in
# control-plane's k3d cluster, where dev / dev-k8s / e2e share one cluster: e2e
# broke while the live binding's subject pointed at control-plane-dev, and the
# workspace-proxy variant of the same collision stalled a resolver informer
# until every request hit a 15s cache-sync wait and Envoy returned 504.
#
# Suffixing means each env fully OWNS its pair, so applies are independent and
# tearing one env down cannot delete another's grants. `forge doctor`'s
# CheckObjectCollision reports the shape this prevents; it warns rather than
# fails, because a deliberately-shared singleton is legitimate at cluster
# scope — but an operator's binding is never one, since its subject is
# per-env by construction.
#
# `service_account` behaves exactly as it does in render_namespaced_rbac
# above: an operator authored as a Service may carry the same override,
# so the ClusterRoleBinding's subject follows the identity the pod binds
# and forge stops emitting an SA it does not own. The failure it prevents
# is worse at this tier than at the namespaced one — a controller-runtime
# manager that cannot read its leases or watch its CRs does not start.
render_cluster_rbac = lambda name: str, namespace: str, rbac: any, pull_secrets: [str] = [], part_of: str = "", service_account: str = "" -> [any] {
    _labels = _managed_labels(name, part_of)
    # Scoped to the namespace for the last-writer-wins reason above. The
    # ServiceAccount is NOT suffixed: it is namespaced already, so its
    # (namespace, name) address is unique without help, and workloads
    # reference it by bare name.
    _cluster_role = "${name}-${namespace}-clusterrole"
    _subject = service_account if service_account else name
    ([] if service_account else [
        {
            apiVersion = "v1"
            kind = "ServiceAccount"
            metadata = {name = name, namespace = namespace, labels = _labels}
            if pull_secrets:
                imagePullSecrets = [{name = s} for s in pull_secrets]
        }
    ]) + [
        {
            apiVersion = "rbac.authorization.k8s.io/v1"
            kind = "ClusterRole"
            metadata = {name = _cluster_role, labels = _labels}
            rules = _default_rules + rbac.rules
        }
        {
            apiVersion = "rbac.authorization.k8s.io/v1"
            kind = "ClusterRoleBinding"
            metadata = {name = "${name}-${namespace}-clusterrolebinding", labels = _labels}
            subjects = [
                {kind = "ServiceAccount", name = _subject, namespace = namespace}
            ]
            roleRef = {
                apiGroup = "rbac.authorization.k8s.io"
                kind = "ClusterRole"
                name = _cluster_role
            }
        }
    ]
}
