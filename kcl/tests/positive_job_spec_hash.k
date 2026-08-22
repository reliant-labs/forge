"""
Positive test — a one-shot Job's name carries a hash of its rendered pod
spec, so re-applying an unchanged Job is a no-op and a changed one creates a
new object.

Context: a Job's `spec.template` is IMMUTABLE. With a fixed name,
re-applying an unchanged Job is not a no-op — the API server rejects it:

    Job.batch "<name>" is invalid: spec.template: ... field is immutable

Because that surfaces at the END of a whole-manifest apply, every Deployment
has already been updated by then, so the deploy reports FAILURE on a rollout
that in fact succeeded. Observed in production: 10 deployments applied, 2
Jobs errored, the job went red. Normalising "the deploy is red but it's
fine" is how a real failure eventually gets waved through.

Handling it out-of-band means delete-then-recreate, and `kubectl delete job
--wait` returns before the object is gone, so the re-apply races the
deletion — which is exactly how the shell workaround failed.

Putting the hash in the name dissolves the problem rather than handling it.
This pins the three properties that make that true:

  * DETERMINISM — same spec, same name. Without this, every deploy creates a
    new Job and the fix is worse than the bug.
  * SENSITIVITY — a changed spec changes the name, or a real change would be
    silently skipped as a no-op.
  * STABLE HANDLE — forge.dev/job-name survives the hash, so the Job is
    still addressable by a name a human wrote.
"""

import forge.workloads as fw

_env = fw.WorkloadEnv {
    namespace = "demo-dev"
    project = "demo"
    registry = "localhost:5050"
    image_tag = "latest"
    network_policies = False
}

_migrate = fw.Workload {
    kind = "job"
    name = "migrate"
    command = ["/app/demo", "db", "migrate", "up"]
}

# Byte-identical declaration, rendered a second time.
_migrate_again = fw.Workload {
    kind = "job"
    name = "migrate"
    command = ["/app/demo", "db", "migrate", "up"]
}

# Same name, DIFFERENT command — a real spec change.
_migrate_changed = fw.Workload {
    kind = "job"
    name = "migrate"
    command = ["/app/demo", "db", "migrate", "down"]
}

_first = [m for m in fw.render_workloads([_migrate], _env) if m.kind == "Job"][0]
_second = [m for m in fw.render_workloads([_migrate_again], _env) if m.kind == "Job"][0]
_changed = [m for m in fw.render_workloads([_migrate_changed], _env) if m.kind == "Job"][0]

# --- DETERMINISM: an unchanged spec renders an identical name ---
# This is what makes the re-apply a true no-op instead of an immutable-field
# error. If this ever fails, every deploy creates a new Job forever.
assert_hash_is_deterministic = _first.metadata.name == _second.metadata.name

# --- SENSITIVITY: a changed spec renders a different name ---
# A new name means a NEW Job object, which is what a changed migration
# command must produce. Equal names here would mean a real change was
# silently skipped as an apply no-op.
assert_hash_tracks_the_spec = _changed.metadata.name != _first.metadata.name

# --- The name is the stable handle plus the hash, and they agree ---
assert_name_shape = _first.metadata.name == "migrate-" + _first.metadata.labels["forge.dev/spec-hash"]
assert_stable_handle = _first.metadata.labels["forge.dev/job-name"] == "migrate"
assert_changed_keeps_handle = _changed.metadata.labels["forge.dev/job-name"] == "migrate"

# --- The ServiceAccount does NOT move with the hash ---
# RBAC is identity, not revision. If the SA name carried the hash, every spec
# change would orphan the old SA and churn the Role/RoleBinding with it —
# and the pod would reference an identity that no longer exists.
_sa = [m for m in fw.render_workloads([_migrate], _env) if m.kind == "ServiceAccount"][0]
_sa_changed = [m for m in fw.render_workloads([_migrate_changed], _env) if m.kind == "ServiceAccount"][0]
assert_sa_is_stable = _sa.metadata.name == "migrate" and _sa_changed.metadata.name == "migrate"
assert_pod_binds_stable_sa = _first.spec.template.spec.serviceAccountName == "migrate"

# --- Superseded Jobs are collected rather than accumulating forever ---
assert_ttl_set = _first.spec.ttlSecondsAfterFinished == 86400

# --- The name is overridable with plain config-unification ---
# No schema field and no knob: a project that needs a fixed name (something
# outside the render referencing it by exact name) writes this. Note `:` and
# not `=` — the `:` form deep-merges and keeps the labels, while `=` replaces
# the whole metadata map and silently drops them.
_pinned = _first | {metadata: {name = "migrate"}}
assert_override_sets_name = _pinned.metadata.name == "migrate"
assert_override_keeps_labels = _pinned.metadata.labels["forge.dev/job-name"] == "migrate"
