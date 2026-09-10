"""
Negative test — a `cron` cannot declare `replicas`.

A CronJob's concurrency is `concurrencyPolicy` on the schedule, not a
replica count. `replicas = 3` on a cron is not three concurrent runs; it
is a value Kubernetes has nowhere to put.

This is the capability class that is easiest to drop silently and hardest
to notice, because the rendered manifest is perfectly valid — it simply
does not contain the thing the author asked for. Someone who writes
`replicas = 3` believes they have scaled the job. Rejecting is what turns
a wrong belief into a message.
"""

import forge.workloads as fw

_bad = fw.Workload {
    name = "sweep"
    kind = "cron"
    schedule = "@hourly"
    replicas = 3
}

manifests = fw.render_workloads([_bad], fw.WorkloadEnv {
    namespace = "demo-dev"
    network_policies = False
})
