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

_managed_labels = lambda name: str -> {str: str} {
    {
        "app.kubernetes.io/name" = name
        "app.kubernetes.io/managed-by" = "forge"
        "app.kubernetes.io/part-of" = name
    }
}

# The default RBAC rules every workload gets — read the projected
# ConfigMap + Secret for its per-env config.
_default_rules: [{str: any}] = [
    {
        apiGroups = [""]
        resources = ["configmaps", "secrets"]
        verbs = ["get", "list", "watch"]
    }
]

# `extra_rules` are PolicyRule dicts appended to the default config-read
# Role (e.g. coordination.k8s.io/leases for a Service running a
# controller-runtime manager). `pull_secrets` are names of EXISTING
# image-pull Secrets; when set they're attached to the generated
# ServiceAccount so every pod/Job that binds it can pull from a private
# registry. Both default to empty — callers that don't need them keep
# the original two-arg behavior and emit identical manifests.
render_namespaced_rbac = lambda name: str, namespace: str, extra_rules: [{str: any}] = [], pull_secrets: [str] = [] -> [any] {
    _labels = _managed_labels(name)
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
render_cluster_rbac = lambda name: str, namespace: str, rbac: any, pull_secrets: [str] = [] -> [any] {
    _labels = _managed_labels(name)
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
