"""
Positive test — `imagePullPolicy` is `Always` for `-dirty` and `latest`
image tags, `IfNotPresent` for clean SHA tags.

Context (Bug 13, 2026-06-08): `forge build` appends `-dirty` to the
git-sha image tag when the worktree has uncommitted changes. The SHA
doesn't move while edits stay uncommitted, so successive rebuilds push
NEW images under the SAME `<sha>-dirty` tag. With the default
`IfNotPresent` policy, k3d/containerd sees the tag locally and runs
the stale layer — `forge deploy` silently lies about what's running.
Pinning `Always` for `-dirty` and `:latest` tags makes the dev loop
correct again.

This test pins:

  * `<sha>-dirty` → Always (the bug case)
  * `:latest`     → Always (k8s default; we set it explicitly so the
                            policy is grep-able from the manifest)
  * clean `<sha>` → IfNotPresent (keeps prod images cacheable)
  * tag containing "dirty" in the middle (`:dirty-feature-v1`)
                  → IfNotPresent (matches only when tag *ends* in -dirty)

Operators and CronJobs share the same `_pull_policy` helper — same
invariants asserted on those too.
"""

import forge

_target = forge.ClusterTarget {
    cluster = "k3d-example"
    namespace = "example-dev"
    registry = "localhost:5051"
}

# Same bundle rendered four times under four different image_tags.
_bundle_for_tag = lambda tag: str -> any {
    forge.Bundle {
        cluster_target = _target
        services = [
            forge.Service {
                name = "api"
                image = "example"
                deploy = _target.deploy | {ports = [8080]}
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
        ]
    }
}

# Pull the CronJob container's imagePullPolicy out — different nesting
# than Service/Operator (jobTemplate.spec.template.spec instead of
# template.spec). Service/Operator share kind="Deployment" so the
# assertions below filter by metadata.name to disambiguate.
_cron_pull_policy = lambda manifests: [any] -> str {
    [m for m in manifests if m.kind == "CronJob"][0].spec.jobTemplate.spec.template.spec.containers[0].imagePullPolicy
}

_dirty_manifests = forge.render_manifests(_bundle_for_tag("1ad9cf1-dirty"), "1ad9cf1-dirty", {}, False)
_latest_manifests = forge.render_manifests(_bundle_for_tag("latest"), "latest", {}, False)
_clean_manifests = forge.render_manifests(_bundle_for_tag("abc1234"), "abc1234", {}, False)
# Tag where "dirty" appears in the middle — must NOT trip the suffix check.
_middir_manifests = forge.render_manifests(_bundle_for_tag("dirty-feature-v1"), "dirty-feature-v1", {}, False)

# ── Invariants ─────────────────────────────────────────────────────

# `<sha>-dirty` → Always on every container kind (Service/Operator/CronJob).
assert_dirty_service = [
    m for m in _dirty_manifests if m.kind == "Deployment" and m.metadata.name == "api"
][0].spec.template.spec.containers[0].imagePullPolicy == "Always"
assert_dirty_operator = [
    m for m in _dirty_manifests if m.kind == "Deployment" and m.metadata.name == "controller"
][0].spec.template.spec.containers[0].imagePullPolicy == "Always"
assert_dirty_cronjob = _cron_pull_policy(_dirty_manifests) == "Always"

# `:latest` → Always (explicit; k8s default already Always for :latest,
# but we render the field so it's grep-able).
assert_latest_service = [
    m for m in _latest_manifests if m.kind == "Deployment" and m.metadata.name == "api"
][0].spec.template.spec.containers[0].imagePullPolicy == "Always"
assert_latest_operator = [
    m for m in _latest_manifests if m.kind == "Deployment" and m.metadata.name == "controller"
][0].spec.template.spec.containers[0].imagePullPolicy == "Always"
assert_latest_cronjob = _cron_pull_policy(_latest_manifests) == "Always"

# Clean SHA tag → IfNotPresent (default behavior preserved; lets the
# kubelet cache layers across prod rollouts).
assert_clean_service = [
    m for m in _clean_manifests if m.kind == "Deployment" and m.metadata.name == "api"
][0].spec.template.spec.containers[0].imagePullPolicy == "IfNotPresent"
assert_clean_operator = [
    m for m in _clean_manifests if m.kind == "Deployment" and m.metadata.name == "controller"
][0].spec.template.spec.containers[0].imagePullPolicy == "IfNotPresent"
assert_clean_cronjob = _cron_pull_policy(_clean_manifests) == "IfNotPresent"

# Suffix check is strict — tag containing "dirty" not at the end stays
# IfNotPresent. Guards against accidentally matching `:dirty-feature-v1`,
# `:no-dirty-here`, etc.
assert_middir_service = [
    m for m in _middir_manifests if m.kind == "Deployment" and m.metadata.name == "api"
][0].spec.template.spec.containers[0].imagePullPolicy == "IfNotPresent"
assert_middir_operator = [
    m for m in _middir_manifests if m.kind == "Deployment" and m.metadata.name == "controller"
][0].spec.template.spec.containers[0].imagePullPolicy == "IfNotPresent"
assert_middir_cronjob = _cron_pull_policy(_middir_manifests) == "IfNotPresent"
