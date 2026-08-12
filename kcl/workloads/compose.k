"""
Forge KCL — workload expansion for docker-compose.

The SECOND lowering of the workload model. `expand.k` lowers workloads
to Kubernetes objects; this file lowers the ONE-SHOT ordering primitive
(`Job.before`) to the docker-compose vocabulary, which expresses it
natively:

    <job>:
      restart: "no"                       # a one-shot must not be restarted
    <dependent>:
      depends_on:
        <job>:
          condition: service_completed_successfully

`service_completed_successfully` is compose's own "wait for exit 0 before
starting me" — the exact semantics `Job.before` declares, enforced by
compose itself rather than by whoever ran the command. It is the reason
`Job` is the portable primitive: the same declaration that becomes an init
container on k8s becomes this on compose, with no loss of meaning.

WHY THIS FILE IS SEPARATE FROM expand.k: expand.k is the k8s ADAPTER and
names k8s concepts throughout (initContainers, securityContext, RBAC). A
target-specific vocabulary belongs in its own adapter, so neither leaks
into the other — the same split kcl/core.k documents for
`_service_to_k8s` vs `_service_to_compose`.

SCOPE — read this before reaching for it. This lowers the ORDERING
primitive, not a whole project. forge's scaffolded `docker-compose.yml`
is a hand-authored file describing dev INFRASTRUCTURE (postgres, the
observability stack, the dev IdP); workloads are not compose services
there, and this file does not make them so. What it gives you is the
fragment for a project that DOES run its workloads under compose: merge
`compose_fragment(workloads)` into your compose document, or read
`compose_depends_on(name, workloads)` for one service's dependency
block.
"""

import .expand as expand

# One compose service dict for a one-shot Job.
#
# `restart: "no"` is the load-bearing key, and it is not the compose
# default (`no` is the default for a bare service, but any project-wide
# `restart: unless-stopped` convention would fight the one-shot). A job
# that is restarted after exiting 0 is no longer a one-shot — it is a
# crash loop that happens to succeed, and every dependent waiting on
# `service_completed_successfully` would be re-gated each time.
_job_service = lambda j: any, image: str -> any {
    {
        "image" = image
        "command" = j.command
        "restart" = "no"
        if j.env:
            "environment" = j.env
    }
}

# The `depends_on` block a workload gets from the jobs that gate it.
# Empty dict when nothing gates it — merge it in unconditionally and a
# workload with no gates renders unchanged.
#
# Returns compose's LONG form (`{condition: ...}`), which is the only form
# that can carry a condition; the short list form means "started", not
# "completed", and would silently give back the race this exists to remove.
#
# `target` is the whole workload, not just its name, because the BROADCAST
# selector (`before = ["*"]`) gates by KIND — see expand.k's `_job_gates`,
# which this delegates to so the two lowerings cannot drift. That sharing
# is the point: a migration job declared once must mean the same thing
# whether the env renders to Kubernetes or to compose.
compose_depends_on = lambda target: any, workloads: [any] -> any {
    {j.name: {"condition" = "service_completed_successfully"} \
        for j in workloads if j.kind == "job" and expand.job_gates(j, target)}
}

# Lower every one-shot job in `workloads` to its compose service, keyed
# by job name. Merge into the `services:` map of a compose document.
#
# UNLIKE the k8s lowering, a gating job DOES render a service object here:
# compose has no init-container concept, so the job IS a top-level service
# and the ordering rides on the dependents' `depends_on`. Same declaration,
# different shape, because the targets differ — which is the whole point of
# lowering rather than templating.
compose_jobs = lambda workloads: [any], image: str -> any {
    {j.name: _job_service(j, image) for j in workloads if j.kind == "job"}
}

# The full compose fragment for a workload set: every job as a service,
# plus the `depends_on` each gated workload needs.
#
# `image` is the project image every workload runs (one image, cobra
# subcommands) — the same resolution the k8s adapter does, passed in
# rather than re-derived so the two cannot drift.
compose_fragment = lambda workloads: [any], image: str -> any {
    _jobs = compose_jobs(workloads, image)
    # Jobs are NOT excluded from the gated set here (they were, when
    # `before` could only enumerate). A broadcast job gates other jobs —
    # a seed must not beat the migration that creates its table — and on
    # compose a job IS a top-level service, so it carries `depends_on`
    # like any other. `_jobs | _gated` merges last-wins, so a gated job's
    # depends_on lands on the service dict `compose_jobs` produced.
    _gated = {c.name: (_jobs[c.name] if c.name in _jobs else {}) \
        | {"depends_on" = compose_depends_on(c, workloads)} \
        for c in workloads if compose_depends_on(c, workloads)}
    {"services" = _jobs | _gated}
}
