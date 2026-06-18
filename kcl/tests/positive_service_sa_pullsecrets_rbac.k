"""
Positive test — three forge.Service rendering gaps the cp-forge launch
exposed, closed together:

  GAP 1 — serviceAccountName on the Service Deployment pod spec.
    render_k8s_service emits a per-Service ServiceAccount + Role +
    RoleBinding (render_namespaced_rbac) but used to NOT set
    `serviceAccountName` on the pod template, so the pod ran as
    `default` and the RoleBinding granted it nothing. Now the pod binds
    its own SA, matching the operator path.

  GAP 2 — imagePullSecrets affordance. K8sCluster.image_pull_secrets
    declares names of EXISTING image-pull Secrets. They're injected onto
    the forge-GENERATED ServiceAccount (the durable spot — covers Jobs
    too) for every workload in the deploy group (Service / Operator /
    CronJob), NOT onto each pod spec. Unset => field omitted.

  GAP 3 — namespaced RBAC affordance for a Service.
    Service.namespaced_rbac appends extra PolicyRules to the Service's
    generated Role (on top of the default configmaps/secrets read), so a
    Service running a controller-runtime manager can be granted e.g.
    coordination.k8s.io/leases without reaching for an Operator.

Also pins the no-op invariants: a Service with neither field set emits a
Role with ONLY the default rule and an SA with NO imagePullSecrets.
"""

import forge

# K8sCluster WITH image_pull_secrets declared (env-wide).
_k8s = forge.K8sCluster {
    cluster = "k3d-example"
    namespace = "example-dev"
    registry = "ghcr.io/reliant/private"
    ports = [8080]
    image_pull_secrets = ["ghcr-creds", "extra-creds"]
}

# Bare K8sCluster — no pull secrets — for the no-op assertions.
_k8s_bare = forge.K8sCluster {
    cluster = "k3d-example"
    namespace = "example-dev"
    registry = "localhost:5051"
    ports = [8080]
}

_bundle = forge.Bundle {
    services = [
        # Service WITH extra namespaced RBAC (controller-runtime manager
        # needs leader-election leases + to read its own pods).
        forge.Service {
            name = "api"
            image = "api-img"
            deploy = _k8s
            namespaced_rbac = [
                {
                    apiGroups = ["coordination.k8s.io"]
                    resources = ["leases"]
                    verbs = ["get", "list", "watch", "create", "update", "patch", "delete"]
                }
                {
                    apiGroups = [""]
                    resources = ["pods"]
                    verbs = ["get", "list", "watch"]
                }
            ]
        }
        # Service WITHOUT namespaced_rbac, on a cluster WITHOUT pull
        # secrets — the no-op baseline.
        forge.Service {
            name = "plain"
            image = "plain-img"
            deploy = _k8s_bare
        }
    ]
    operators = [
        forge.Operator {
            name = "controller"
            image = "controller-img"
            crds = ["widgets.example.com"]
            cluster_rbac = forge.ClusterRBAC {
                rules = [{apiGroups = [""], resources = ["pods"], verbs = ["get"]}]
            }
        }
    ]
    cronjobs = [
        forge.CronJob {
            name = "cleanup"
            schedule = "0 * * * *"
            image = "cleanup-img"
            command = ["/cleanup"]
        }
        # schedule == "" renders as a one-shot Job (the migrate-Job
        # pattern). It MUST bind its own SA so it inherits the cluster's
        # imagePullSecrets — otherwise the Job runs as `default` and
        # ImagePullBackOffs on a private registry. Pins the render_cronjob
        # serviceAccountName fix.
        forge.CronJob {
            name = "migrate"
            schedule = ""
            image = "migrate-img"
            command = ["/migrate"]
        }
    ]
}

# Render the bundle WITH pull secrets (the `api` service uses _k8s).
_env = forge.RenderEnv {
    cluster = _k8s
    image_tag = "abc1234"
    network_policies = False
}

_manifests = forge.render_manifests(_bundle, _env)

# ── Manifest selectors ─────────────────────────────────────────────

_api_deploy = [m for m in _manifests if m.kind == "Deployment" and m.metadata.name == "api"][0]
_api_sa = [m for m in _manifests if m.kind == "ServiceAccount" and m.metadata.name == "api"][0]
_api_role = [m for m in _manifests if m.kind == "Role" and m.metadata.name == "api-role"][0]

_plain_sa = [m for m in _manifests if m.kind == "ServiceAccount" and m.metadata.name == "plain"][0]
_plain_role = [m for m in _manifests if m.kind == "Role" and m.metadata.name == "plain-role"][0]

_op_sa = [m for m in _manifests if m.kind == "ServiceAccount" and m.metadata.name == "controller"][0]
_cron_sa = [m for m in _manifests if m.kind == "ServiceAccount" and m.metadata.name == "cleanup"][0]

# Scheduled CronJob (kind: CronJob) and one-shot Job (kind: Job, from
# schedule == "") pods, plus the one-shot Job's generated SA.
_cron_cj = [m for m in _manifests if m.kind == "CronJob" and m.metadata.name == "cleanup"][0]
_migrate_job = [m for m in _manifests if m.kind == "Job" and m.metadata.name == "migrate"][0]
_migrate_sa = [m for m in _manifests if m.kind == "ServiceAccount" and m.metadata.name == "migrate"][0]

# ── GAP 1: serviceAccountName on the Service Deployment ─────────────

assert_service_binds_sa = _api_deploy.spec.template.spec.serviceAccountName == "api"

# ── GAP 2: imagePullSecrets on the generated SA (not the pod spec) ──

# The env declares image_pull_secrets, so every workload's SA carries it.
assert_api_sa_pull = _api_sa.imagePullSecrets == [{name = "ghcr-creds"}, {name = "extra-creds"}]
assert_op_sa_pull = _op_sa.imagePullSecrets == [{name = "ghcr-creds"}, {name = "extra-creds"}]
assert_cron_sa_pull = _cron_sa.imagePullSecrets == [{name = "ghcr-creds"}, {name = "extra-creds"}]
# The one-shot migrate Job's generated SA carries the pull secrets too.
assert_migrate_sa_pull = _migrate_sa.imagePullSecrets == [{name = "ghcr-creds"}, {name = "extra-creds"}]
# It is injected on the SA, NOT on the pod spec — the durable choice.
assert_pull_not_on_pod = _api_deploy.spec.template.spec.imagePullSecrets == Undefined

# ── render_cronjob fix: serviceAccountName on Job/CronJob pods ───────

# The one-shot Job pod binds its own SA → inherits the pull secrets the
# SA carries (no more ImagePullBackOff as `default`).
assert_migrate_job_binds_sa = _migrate_job.spec.template.spec.serviceAccountName == "migrate"
# The scheduled CronJob's jobTemplate pod binds its SA as well.
assert_cron_cj_binds_sa = _cron_cj.spec.jobTemplate.spec.template.spec.serviceAccountName == "cleanup"

# ── GAP 3: extra namespaced RBAC rules on the Service Role ──────────

# Default config-read rule is preserved as the first rule, the two
# declared rules are appended after it.
assert_api_role_default_first = _api_role.rules[0] == {
    apiGroups = [""]
    resources = ["configmaps", "secrets"]
    verbs = ["get", "list", "watch"]
}
assert_api_role_len = len(_api_role.rules) == 3
assert_api_role_leases = _api_role.rules[1].apiGroups == ["coordination.k8s.io"]
assert_api_role_leases_res = _api_role.rules[1].resources == ["leases"]
assert_api_role_pods = _api_role.rules[2].resources == ["pods"]

# ── No-op invariants: a bare Service emits the original manifests ───

# `plain` Service: namespaced_rbac unset → Role has ONLY the default rule.
assert_plain_role_default_only = _plain_role.rules == [{
    apiGroups = [""]
    resources = ["configmaps", "secrets"]
    verbs = ["get", "list", "watch"]
}]
# `plain` Service still binds its own SA (GAP 1 applies unconditionally).
_plain_deploy = [m for m in _manifests if m.kind == "Deployment" and m.metadata.name == "plain"][0]
assert_plain_binds_sa = _plain_deploy.spec.template.spec.serviceAccountName == "plain"

# Render a SECOND time on the bare cluster (no pull secrets) to pin the
# imagePullSecrets no-op: the SA must omit the field entirely.
_env_bare = forge.RenderEnv {
    cluster = _k8s_bare
    image_tag = "abc1234"
    network_policies = False
}
_bundle_bare = forge.Bundle {
    services = [forge.Service {name = "plain", image = "plain-img", deploy = _k8s_bare}]
}
_manifests_bare = forge.render_manifests(_bundle_bare, _env_bare)
_plain_sa_bare = [m for m in _manifests_bare if m.kind == "ServiceAccount" and m.metadata.name == "plain"][0]
assert_no_pull_secrets_when_unset = _plain_sa_bare.imagePullSecrets == Undefined
