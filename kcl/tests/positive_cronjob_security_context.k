"""
Positive test — CronJob and one-shot Job pod templates carry the same
restricted-PSA-compliant securityContext as Deployments/Operators.

Context (2026-06-16): `render_cronjob` built its pod spec WITHOUT a
pod-level or container-level securityContext, while `render_k8s_service`
and `render_operator` apply `_default_pod_security_context` and
`_default_container_security_context`. Under a namespace enforcing the
`restricted` Pod Security Standard, the Job/CronJob pod is REJECTED at
admission (FailedCreate, pod never created) because it lacks
runAsNonRoot, seccompProfile=RuntimeDefault, capabilities.drop=[ALL],
allowPrivilegeEscalation=false, etc. This surfaced launching a real
project whose `cp-forge-migrate` one-shot Job (a forge.CronJob with
schedule="") could not create a pod under `restricted:latest`.

This test pins the restricted baseline on BOTH render paths:

  * scheduled CronJob  → spec.jobTemplate.spec.template.spec
  * one-shot Job       → spec.template.spec
"""

import forge

_k8s = forge.K8sCluster {
    cluster = "k3d-example"
    namespace = "example-dev"
    registry = "localhost:5051"
    ports = [8080]
}

_bundle = forge.Bundle {
    cronjobs = [
        forge.CronJob {
            name = "scheduled-sweep"
            schedule = "0 * * * *"
            image = "sweep-img"
            command = ["/sweep"]
        }
        # schedule = "" renders as a one-shot Job, not a CronJob.
        forge.CronJob {
            name = "migrate"
            image = "migrate-img"
            command = ["/migrate", "up"]
        }
    ]
}

_env = forge.RenderEnv {
    cluster = _k8s
    image_tag = "abc1234"
    network_policies = False
}

_manifests = forge.render_manifests(_bundle, _env)

# Pull the scheduled CronJob's pod + container securityContext. The pod
# template nests under spec.jobTemplate.spec.template.spec for a CronJob.
_cron = [m for m in _manifests if m.kind == "CronJob"][0]
_cron_pod_spec = _cron.spec.jobTemplate.spec.template.spec
_cron_pod_sc = _cron_pod_spec.securityContext
_cron_ctr_sc = _cron_pod_spec.containers[0].securityContext

# Pull the one-shot Job's pod + container securityContext. A Job nests
# the pod template directly under spec.template.spec (no jobTemplate).
_job = [m for m in _manifests if m.kind == "Job"][0]
_job_pod_spec = _job.spec.template.spec
_job_pod_sc = _job_pod_spec.securityContext
_job_ctr_sc = _job_pod_spec.containers[0].securityContext

# ── Invariants ─────────────────────────────────────────────────────

# The one-shot path (schedule == "") really renders kind: Job.
assert_oneshot_is_job = _job.metadata.name == "migrate"
assert_scheduled_is_cronjob = _cron.metadata.name == "scheduled-sweep"

# securityContext is present and non-empty on both pod templates.
assert_cron_pod_sc_present = _cron_pod_sc != Undefined and _cron_pod_sc != {}
assert_cron_ctr_sc_present = _cron_ctr_sc != Undefined and _cron_ctr_sc != {}
assert_job_pod_sc_present = _job_pod_sc != Undefined and _job_pod_sc != {}
assert_job_ctr_sc_present = _job_ctr_sc != Undefined and _job_ctr_sc != {}

# Pod-level restricted fields — scheduled CronJob.
assert_cron_pod_nonroot = _cron_pod_sc.runAsNonRoot == True
assert_cron_pod_runasuser = _cron_pod_sc.runAsUser == 65532
assert_cron_pod_seccomp = _cron_pod_sc.seccompProfile.type == "RuntimeDefault"

# Container-level restricted fields — scheduled CronJob.
assert_cron_ctr_drop_all = _cron_ctr_sc.capabilities.drop == ["ALL"]
assert_cron_ctr_no_priv_esc = _cron_ctr_sc.allowPrivilegeEscalation == False
assert_cron_ctr_ro_rootfs = _cron_ctr_sc.readOnlyRootFilesystem == True

# Pod-level restricted fields — one-shot Job (the cp-forge-migrate case).
assert_job_pod_nonroot = _job_pod_sc.runAsNonRoot == True
assert_job_pod_runasuser = _job_pod_sc.runAsUser == 65532
assert_job_pod_seccomp = _job_pod_sc.seccompProfile.type == "RuntimeDefault"

# Container-level restricted fields — one-shot Job.
assert_job_ctr_drop_all = _job_ctr_sc.capabilities.drop == ["ALL"]
assert_job_ctr_no_priv_esc = _job_ctr_sc.allowPrivilegeEscalation == False
assert_job_ctr_ro_rootfs = _job_ctr_sc.readOnlyRootFilesystem == True

# The Job/CronJob securityContext must MATCH the Deployment/Operator
# baseline so admission behaves identically across workload kinds.
assert_cron_matches_job_pod_sc = _cron_pod_sc == _job_pod_sc
assert_cron_matches_job_ctr_sc = _cron_ctr_sc == _job_ctr_sc
