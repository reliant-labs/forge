"""
Positive test — a one-shot Job states its DEPLOY PHASE on its metadata.

`forge env deploy` applies a pre-rollout Job, and waits for it to COMPLETE,
before it applies any workload in the same deploy; a post-rollout Job rides
with the workloads. Go reads the phase from the `forge.dev/deploy-phase`
annotation on the rendered Job, so the render must always state it:

  * undeclared  → "pre-rollout" (the default: a migration precedes the code
                  that needs it)
  * declared    → exactly what was declared
  * both render paths — the bundle's forge.CronJob{schedule=""} and the
    workloads path's kind="job" — say the same thing

The annotation must sit on the Job's METADATA, not its pod template: the
Job's name is a hash of the pod spec, and declaring a phase must not rename
(and so re-run) a Job that already completed.
"""

import forge
import forge.workloads as fw

_target = forge.ClusterTarget {
    cluster = "k3d-example"
    namespace = "example-prod"
    registry = "localhost:5051"
}

_bundle = forge.Bundle {
    cluster_target = _target
    cronjobs = [
        forge.CronJob {
            name = "migrate"
            image = "app"
            command = ["/app", "db", "migrate", "up"]
        }
        forge.CronJob {
            name = "idp-provision"
            image = "app"
            command = ["/app", "idp", "provision"]
            deploy_phase = "post-rollout"
        }
        forge.CronJob {
            name = "backfill"
            image = "app"
            command = ["/app", "backfill"]
            deploy_phase = "pre-rollout"
        }
    ]
}

_manifests = forge.render_manifests(_bundle, "abc1234", {}, False)
_jobs = {m.metadata.labels["forge.dev/job-name"]: m for m in _manifests if m.kind == "Job"}

assert_bundle_default_is_pre = _jobs["migrate"].metadata.annotations["forge.dev/deploy-phase"] == "pre-rollout"
assert_bundle_post_declared = _jobs["idp-provision"].metadata.annotations["forge.dev/deploy-phase"] == "post-rollout"
assert_bundle_pre_declared = _jobs["backfill"].metadata.annotations["forge.dev/deploy-phase"] == "pre-rollout"

# The phase is metadata only: the pod template carries no annotation, and the
# name is still the spec-hash form, so an unchanged Job keeps its name.
assert_bundle_pod_template_untouched = "annotations" not in _jobs["idp-provision"].spec.template.metadata
assert_bundle_name_is_hash = _jobs["migrate"].metadata.name == "migrate-" + _jobs["migrate"].metadata.labels["forge.dev/spec-hash"]

# The same Job declared with and without a phase hashes identically: the
# phase must not rename a Job that already ran.
_plain = forge.render_manifests(forge.Bundle {
    cluster_target = _target
    cronjobs = [forge.CronJob {name = "idp-provision", image = "app", command = ["/app", "idp", "provision"]}]
}, "abc1234", {}, False)
_plain_job = [m for m in _plain if m.kind == "Job"][0]
assert_phase_does_not_move_the_hash = _plain_job.metadata.name == _jobs["idp-provision"].metadata.name

# --- The workloads path says the same thing ---
_wl = fw.render_workloads([
    fw.Workload {kind = "service", name = "api"}
    fw.Workload {kind = "job", name = "seed", command = ["/app", "seed"]}
    fw.Workload {kind = "job", name = "warm", command = ["/app", "warm"], deploy_phase = "post-rollout"}
], fw.WorkloadEnv {
    namespace = "example-prod"
    project = "example"
    registry = "localhost:5051"
    image_tag = "abc1234"
    network_policies = False
})
_wl_jobs = {m.metadata.labels["forge.dev/job-name"]: m for m in _wl if m.kind == "Job"}
assert_workload_default_is_pre = _wl_jobs["seed"].metadata.annotations["forge.dev/deploy-phase"] == "pre-rollout"
assert_workload_post_declared = _wl_jobs["warm"].metadata.annotations["forge.dev/deploy-phase"] == "post-rollout"
