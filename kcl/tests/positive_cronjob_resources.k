"""
Positive test — CronJob and one-shot Job containers carry cpu/memory
requests and limits, defaulted like a Service's and overridable per job.

Context: `render_cronjob` emitted no `resources` block at all, while
`render_k8s_service` defaults one (_default_resources) for any Service
that does not declare its own. A container with no resources is
BestEffort QoS — the FIRST thing the kubelet evicts under node pressure.

That is the wrong QoS for the workload this kind most often expresses.
`schedule = ""` renders a one-shot Job, and the canonical one-shot Job is
the schema migration that gates a release: evicted mid-run, it leaves a
release half-applied against a partially-migrated database.

It was also unfixable from a project: forge.CronJob had no `resources`
field, so `forge doctor`'s Deploy Resources check failed a project on a
container the project had no knob to size. This pins both halves — the
default, and the override.

  * scheduled CronJob  -> spec.jobTemplate.spec.template.spec
  * one-shot Job       -> spec.template.spec
"""

import forge

_target = forge.ClusterTarget {
    cluster = "k3d-example"
    namespace = "example-dev"
    registry = "localhost:5051"
}

_bundle = forge.Bundle {
    cluster_target = _target
    cronjobs = [
        # No `resources` — must still render the shared defaults.
        forge.CronJob {
            name = "scheduled-sweep"
            schedule = "0 * * * *"
            image = "sweep-img"
            command = ["/sweep"]
        }
        # An explicit sizing must survive untouched: a migration against a
        # large table is exactly the job that needs more than the default.
        forge.CronJob {
            name = "migrate"
            image = "migrate-img"
            command = ["/migrate", "up"]
            resources = forge.Resources {
                cpu_request_millicores = forge.cpu(0.5)
                cpu_limit_millicores = forge.cpu(2)
                memory_request_bytes = forge.mib(512)
                memory_limit_bytes = forge.gib(2)
            }
        }
    ]
}

_manifests = forge.render_manifests(_bundle, "abc1234", {}, False)

_cron_ctr = [m for m in _manifests if m.kind == "CronJob"][0].spec.jobTemplate.spec.template.spec.containers[0]
_job_ctr = [m for m in _manifests if m.kind == "Job"][0].spec.template.spec.containers[0]

# ── Invariants ─────────────────────────────────────────────────────

# Defaulted: the scheduled CronJob declared no resources and must still
# get the same profile a Service does (100m/500m, 128Mi/512Mi).
assert_cron_has_resources = _cron_ctr.resources != Undefined and _cron_ctr.resources != {}
assert_cron_cpu_request = _cron_ctr.resources.requests.cpu == "100m"
assert_cron_cpu_limit = _cron_ctr.resources.limits.cpu == "500m"
assert_cron_mem_request = _cron_ctr.resources.requests.memory == "128Mi"
assert_cron_mem_limit = _cron_ctr.resources.limits.memory == "512Mi"

# Overridden: the one-shot Job's explicit sizing reaches the manifest
# unchanged — a default that quietly won would be worse than no default.
assert_job_has_resources = _job_ctr.resources != Undefined and _job_ctr.resources != {}
assert_job_cpu_request = _job_ctr.resources.requests.cpu == "500m"
assert_job_cpu_limit = _job_ctr.resources.limits.cpu == "2"
assert_job_mem_request = _job_ctr.resources.requests.memory == "512Mi"
assert_job_mem_limit = _job_ctr.resources.limits.memory == "2Gi"
