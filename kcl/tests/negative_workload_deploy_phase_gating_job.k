"""
Negative test — Workload.deploy_phase on a GATING job (one with `before`) is
refused.

A job that declares `before` renders no standalone Job: it becomes an
initContainer on each dependent, so the pod ordering already IS its phase.
A deploy_phase there could never be honoured, so it is an error, not a no-op.
"""

import forge.workloads as fw

_j = fw.Workload {
    kind = "job"
    name = "migrate"
    command = ["/app", "db", "migrate", "up"]
    before = [fw.BEFORE_ALL]
    deploy_phase = "post-rollout"
}
