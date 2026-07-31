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

# `part_of` is the PROJECT these objects belong to (the k8s
# recommended-labels "higher level application"), so a CD pipeline can
# select every object of one release with
# `-l app.kubernetes.io/part-of=<project>`. Empty falls back to the
# object's own name for callers with no project context.
_managed_labels = lambda name: str, part_of: str = "" -> {str: str} {
    {
        "app.kubernetes.io/name" = name
        "app.kubernetes.io/managed-by" = "forge"
        "app.kubernetes.io/part-of" = part_of if part_of else name
    }
}

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
render_namespaced_rbac = lambda name: str, namespace: str, extra_rules: [{str: any}] = [], pull_secrets: [str] = [], part_of: str = "" -> [any] {
    _labels = _managed_labels(name, part_of)
    [
        {
            apiVersion = "v1"
            kind = "ServiceAccount"
            metadata = {name = name, namespace = namespace, labels = _labels}
            if pull_secrets:
                imagePullSecrets = [{name = s} for s in pull_secrets]
        }
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
                {kind = "ServiceAccount", name = name, namespace = namespace}
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
render_cluster_rbac = lambda name: str, namespace: str, rbac: any, pull_secrets: [str] = [], part_of: str = "" -> [any] {
    _labels = _managed_labels(name, part_of)
    [
        {
            apiVersion = "v1"
            kind = "ServiceAccount"
            metadata = {name = name, namespace = namespace, labels = _labels}
            if pull_secrets:
                imagePullSecrets = [{name = s} for s in pull_secrets]
        }
        {
            apiVersion = "rbac.authorization.k8s.io/v1"
            kind = "ClusterRole"
            metadata = {name = "${name}-clusterrole", labels = _labels}
            rules = _default_rules + rbac.rules
        }
        {
            apiVersion = "rbac.authorization.k8s.io/v1"
            kind = "ClusterRoleBinding"
            metadata = {name = "${name}-clusterrolebinding", labels = _labels}
            subjects = [
                {kind = "ServiceAccount", name = name, namespace = namespace}
            ]
            roleRef = {
                apiGroup = "rbac.authorization.k8s.io"
                kind = "ClusterRole"
                name = "${name}-clusterrole"
            }
        }
    ]
}
