"""
Negative test — CronJob.deploy_phase on a SCHEDULED CronJob is refused.

The phase orders a one-shot Job against the deploy's workloads. A scheduled
CronJob runs on its own cadence and nothing waits for it, so the declaration
would be silently meaningless — which is exactly the kind of dropped
declaration forge refuses rather than ignores.
"""

import forge

_c = forge.CronJob {
    name = "nightly"
    image = "app"
    schedule = "@daily"
    command = ["/app", "report"]
    deploy_phase = "pre-rollout"
}
