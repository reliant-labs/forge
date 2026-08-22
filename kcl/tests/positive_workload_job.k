"""
Positive test — the one-shot `Job` component kind and its ORDERING
declaration (`before`).

Covers both k8s lowerings:

  * a Job that gates workloads (`before` non-empty) renders NO standalone
    Job object; it becomes an initContainer on each named dependent, which
    is the only ordering Kubernetes itself enforces.
  * a Job that gates nothing renders a standalone batch/v1 Job.

Each `assert_*` must evaluate true.
"""

import forge.workloads as fw

_components = [
    fw.Workload {
        kind = "service"
        name = "api"
        env = {"LOG_LEVEL" = "info"}
    }
    fw.Workload {kind = "worker", name = "sync"}
    # BROADCAST: gates every workload without naming one. Declared FIRST,
    # which is what puts it ahead of provision-idp in the init list.
    fw.Workload {
        kind = "job"
        name = "migrate"
        command = ["/app/demo", "db", "migrate", "up"]
        before = [fw.BEFORE_ALL]
    }
    # Gates BOTH workloads — one job, two dependents.
    fw.Workload {
        kind = "job"
        name = "provision-idp"
        command = ["/app/demo", "provision-idp"]
        env = {"IDP_URL" = "http://idp:8080"}
        before = ["api", "sync"]
    }
    # Gates nothing — a bare one-shot.
    fw.Workload {
        kind = "job"
        name = "seed"
        command = ["/app/demo", "seed"]
    }
]

_env = fw.WorkloadEnv {
    namespace = "demo-dev"
    project = "demo"
    registry = "localhost:5050"
    image_tag = "latest"
    network_policies = False
}

manifests = fw.render_workloads(_components, _env)

# --- A gating Job renders NO standalone Job object ---
# Rendering one would run the command twice: once unordered as a Job,
# once ordered as an init container. The unordered copy IS the race
# `before` exists to remove.
_job_objs = [m for m in manifests if m.kind == "Job"]
# Select on forge.dev/job-name, not metadata.name: the rendered name carries a
# spec hash (`seed-<hash>`), which is what makes re-applying an unchanged Job a
# no-op instead of an immutable-field error. The label is the stable handle
# that survives that, and this test selecting through it is the same move a
# `kubectl get job -l forge.dev/job-name=seed` makes.
_job_handles = [m.metadata.labels["forge.dev/job-name"] for m in _job_objs]
assert_gating_job_has_no_job_object = "provision-idp" not in _job_handles

# --- A non-gating Job DOES render a standalone Job ---
assert_bare_job_renders = "seed" in _job_handles
_seed = [m for m in _job_objs if m.metadata.labels["forge.dev/job-name"] == "seed"][0]

# The name is the stable handle plus a hash suffix — never the bare name.
assert_seed_name_is_hashed = _seed.metadata.name.startswith("seed-") and _seed.metadata.name != "seed"
assert_seed_hash_label_matches = _seed.metadata.name == "seed-" + _seed.metadata.labels["forge.dev/spec-hash"]
assert_bare_job_api = _seed.apiVersion == "batch/v1"
assert_bare_job_command = _seed.spec.template.spec.containers[0].command == ["/app/demo", "seed"]
assert_bare_job_restart = _seed.spec.template.spec.restartPolicy == "OnFailure"
# A one-shot must be able to give up; retrying a broken command forever
# is not "eventually consistent", it is a wedged rollout with no signal.
assert_bare_job_backoff = _seed.spec.backoffLimit == 6

# --- The gating Job becomes an initContainer on EACH dependent ---
_api = [m for m in manifests if m.kind == "Deployment" and m.metadata.name == "api"][0]
_sync = [m for m in manifests if m.kind == "Deployment" and m.metadata.name == "sync"][0]
_api_inits = [c.name for c in _api.spec.template.spec.initContainers]
_sync_inits = [c.name for c in _sync.spec.template.spec.initContainers]
assert_api_gated = "provision-idp" in _api_inits
assert_sync_gated = "provision-idp" in _sync_inits

# The job runs its OWN command and env, not the dependent's.
_api_job_init = [c for c in _api.spec.template.spec.initContainers if c.name == "provision-idp"][0]
assert_init_runs_job_command = _api_job_init.command == ["/app/demo", "provision-idp"]
assert_init_carries_job_env = len([e for e in _api_job_init.env if e.name == "IDP_URL"]) == 1
# ...and the dependent's own env is NOT leaked into it (the job is a
# different program; it declares what it needs).
assert_init_env_is_the_jobs = len([e for e in _api_job_init.env if e.name == "LOG_LEVEL"]) == 0

# --- Ordering between two gates ---
# migrate is a BROADCAST job declared ahead of provision-idp, so it runs
# first and the provisioner sees a current schema. k8s runs
# initContainers in list order, and that list is workload declaration
# order — so where you put the declaration IS the ordering contract.
assert_migrate_precedes_job = _api_inits == ["migrate", "provision-idp"]

# --- A Job that gates nothing does not gate anything by accident ---
assert_seed_gates_nothing = "seed" not in _api_inits and "seed" not in _sync_inits

# --- A broadcast job gates OTHER jobs, but never itself ---
# `seed` gates nothing, so it renders a standalone Job — and that Job's
# pod must still wait for the migration, or it writes rows to a table
# that does not exist yet.
_seed_inits = [c.name for c in _seed.spec.template.spec.initContainers]
assert_bare_job_is_gated_by_broadcast = _seed_inits == ["migrate"]
# The broadcast job itself is lowered onto its dependents, so it renders
# no standalone Job of its own — and cannot be its own init container.
assert_broadcast_job_has_no_job_object = "migrate" not in _job_handles

# --- Jobs bind their own ServiceAccount, like every other workload ---
_sa_names = [m.metadata.name for m in manifests if m.kind == "ServiceAccount"]
assert_job_has_sa = "seed" in _sa_names and "provision-idp" in _sa_names
assert_bare_job_binds_sa = _seed.spec.template.spec.serviceAccountName == "seed"

# --- Labels follow the same project/component split as every kind ---
assert_job_part_of_project = _seed.metadata.labels["app.kubernetes.io/part-of"] == "demo"
assert_job_name_is_component = _seed.metadata.labels["app.kubernetes.io/name"] == "seed"
