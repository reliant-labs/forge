"""
Negative test — CronJob.schedule must be either '' (one-shot Job),
a named expression ('@hourly', ...), or a 5-field cron expression.
Anything else fails the check block.
"""

import forge

_cron = forge.CronJob {
    name = "bad-schedule"
    image = "x"
    schedule = "every other tuesday"
}
