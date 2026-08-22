"""
Positive test — `app.kubernetes.io/part-of` names the PROJECT on the
BUNDLE render path (forge.render_manifests -> lib/services.k).

WHY THIS FIXTURE EXISTS SEPARATELY FROM positive_workloads.k
============================================================

positive_workloads.k already asserted part-of-is-the-project — but only
for the WORKLOAD path (fw.render_workloads -> workloads/expand.k). Forge
had a SECOND label renderer in lib/services.k, which is the path a
Bundle-based project renders through, and it hardcoded
`part-of = <workload's own name>`. The workload-path test passed the
whole time the Bundle path was broken, so a real project's deployments
shipped labelled with their own names.

What that cost: a CD pipeline's rollout gate selects one release's
workloads with `kubectl get deployments -l app.kubernetes.io/part-of=<project>`.
Against control-plane's prod render that selector matched 5 of 13
Deployments — only the ones whose label the project hardcoded in its own
KCL. The "wait for rollouts" step iterated the 8 it could not see, waited
on NOTHING, and reported a green deploy while three deployments never
rolled.

So the assertion that matters is not "the label renders" — it is "the
CD SELECTOR finds every Deployment", which is what this file checks, on
the path a real project actually uses.

The Bundle here deliberately mixes every builder that stamps the label:
a plain Service, an operator-flavored Service (cluster RBAC), a real
Operator, a scheduled CronJob, a one-shot Job (schedule == ""), an
image-less infra service carrying owned raw manifests, a ConfigMap and a
RuntimeClass. Each one is a distinct code path in lib/services.k /
lib/rbac.k / render.k, and each was a separate opportunity for a copy of
the label renderer to drift.
"""

import forge

_project = "demo-project"

_target = forge.ClusterTarget {
    cluster = "k3d-example"
    namespace = "example-prod"
    registry = "ghcr.io/reliant/example"
}

_bundle = forge.Bundle {
    project = _project
    cluster_target = _target
    config_maps = [forge.ConfigMap {name = "app-config", data = {"KEY" = "value"}}]
    runtime_classes = [forge.RuntimeClass {name = "gvisor", handler = "runc"}]
    services = [
        forge.RenderedWorkload {
            name = "api"
            image = "api-img"
            deploy = _target.deploy | {ports = [8080]}
        }
        # An operator-flavored Service: takes the CLUSTER-RBAC branch of
        # _render_cluster_service rather than the namespaced one.
        forge.RenderedWorkload {
            name = "controller"
            image = "controller-img"
            deploy = _target.deploy | {ports = [8080]}
            operator_cluster_rbac = [
                {apiGroups = [""], resources = ["pods"], verbs = ["get", "list"]}
            ]
        }
        # Image-less INFRA service: contributes no Deployment, only its
        # owned raw manifests (the render_owned_manifests stamping path).
        forge.RenderedWorkload {
            name = "infra"
            deploy = _target.deploy
            manifests = [
                {
                    apiVersion = "v1"
                    kind = "Secret"
                    metadata = {name = "bootstrap", namespace = "example-prod"}
                }
            ]
        }
    ]
    operators = [
        forge.Operator {
            name = "widget-operator"
            image = "operator-img"
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
        # schedule == "" renders a one-shot Job, a distinct branch again.
        forge.CronJob {
            name = "migrate"
            schedule = ""
            image = "migrate-img"
            command = ["/migrate"]
        }
    ]
}

_manifests = forge.render_manifests(_bundle, "abc1234", {}, False)

# ── THE CD SELECTOR ────────────────────────────────────────────────
#
# This is the assertion the bug would have failed: simulate
# `kubectl get deployments -l app.kubernetes.io/part-of=<project>` and
# require it to match EVERY Deployment, not a subset.

_deployments = [m for m in _manifests if m.kind == "Deployment"]
_selected = [
    m for m in _deployments
    if m.metadata.labels["app.kubernetes.io/part-of"] == _project
]

# Guard against the vacuous pass: a selector over an EMPTY set trivially
# "matches all", which is the exact failure mode (an empty `for` exits 0
# and reports success) this whole fixture exists to prevent.
assert_deployments_exist = len(_deployments) == 3
assert_selector_matches_every_deployment = len(_selected) == len(_deployments)

# Same for Services — the CD pipeline's endpoint-readiness step selects
# those with the identical label.
_services = [m for m in _manifests if m.kind == "Service"]
_services_selected = [
    m for m in _services if m.metadata.labels["app.kubernetes.io/part-of"] == _project
]
assert_services_exist = len(_services) > 0
assert_selector_matches_every_service = len(_services_selected) == len(_services)

# ── Every labelled object in the bundle carries the project ─────────
#
# Gateways/routes are deliberately excluded: ingress topology groups
# under the fixed "forge-ingress", not under the app project (see
# lib/gateway.k). This bundle declares none, so the sweep is total.

_labelled = [m for m in _manifests if m.metadata.labels]
_wrong = [
    m for m in _labelled
    if "app.kubernetes.io/part-of" in m.metadata.labels and \
        m.metadata.labels["app.kubernetes.io/part-of"] != _project
]
assert_no_object_labels_itself = len(_labelled) > 0 and len(_wrong) == 0

# Pod templates carry it too, so a release's PODS are selectable.
_api_deploy = [m for m in _deployments if m.metadata.name == "api"][0]
assert_pod_template_part_of = \
    _api_deploy.spec.template.metadata.labels["app.kubernetes.io/part-of"] == _project

# The one-shot Job and the scheduled CronJob (separate builders).
_migrate_job = [m for m in _manifests if m.kind == "Job"][0]
assert_job_part_of = _migrate_job.metadata.labels["app.kubernetes.io/part-of"] == _project
_cron = [m for m in _manifests if m.kind == "CronJob"][0]
assert_cron_part_of = _cron.metadata.labels["app.kubernetes.io/part-of"] == _project

# RBAC objects (lib/rbac.k), both tiers.
_api_role = [m for m in _manifests if m.kind == "Role" and m.metadata.name == "api-role"][0]
assert_role_part_of = _api_role.metadata.labels["app.kubernetes.io/part-of"] == _project
_cluster_role = [m for m in _manifests if m.kind == "ClusterRole"][0]
assert_cluster_role_part_of = \
    _cluster_role.metadata.labels["app.kubernetes.io/part-of"] == _project

# The image-less infra service's owned raw manifest, stamped on the way out.
_bootstrap = [m for m in _manifests if m.kind == "Secret" and m.metadata.name == "bootstrap"][0]
assert_owned_manifest_part_of = \
    _bootstrap.metadata.labels["app.kubernetes.io/part-of"] == _project

# Env-shared resources (no workload `name` label) still name the project.
_cm = [m for m in _manifests if m.kind == "ConfigMap"][0]
assert_config_map_part_of = _cm.metadata.labels["app.kubernetes.io/part-of"] == _project
_rc = [m for m in _manifests if m.kind == "RuntimeClass"][0]
assert_runtime_class_part_of = _rc.metadata.labels["app.kubernetes.io/part-of"] == _project

# ── `name` and `part-of` must NOT collapse into each other ──────────
#
# Setting both to the project would satisfy every assertion above while
# destroying the per-workload group `--target` selects on. `name` stays
# the WORKLOAD.
assert_name_is_workload = _api_deploy.metadata.labels["app.kubernetes.io/name"] == "api"
assert_selector_is_workload = \
    _api_deploy.spec.selector.matchLabels["app.kubernetes.io/name"] == "api"

# ── Fallback: a Bundle with NO project renders as it always did ─────
#
# `project` defaults to "" and each object then labels itself, so adding
# the field cannot change an existing project's manifests until it opts
# in. This is what makes the fix inert-by-default.

_bundle_no_project = forge.Bundle {
    cluster_target = _target
    services = [
        forge.RenderedWorkload {
            name = "api"
            image = "api-img"
            deploy = _target.deploy | {ports = [8080]}
        }
    ]
}
_mf_no_project = forge.render_manifests(_bundle_no_project, "abc1234", {}, False)
_api_no_project = [m for m in _mf_no_project if m.kind == "Deployment"][0]
assert_unset_project_falls_back_to_name = \
    _api_no_project.metadata.labels["app.kubernetes.io/part-of"] == "api"
