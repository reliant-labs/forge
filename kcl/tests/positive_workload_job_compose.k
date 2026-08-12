"""
Positive test — the COMPOSE lowering of the one-shot `Job` primitive.

The same `before` declaration that becomes an initContainer under
Kubernetes becomes `depends_on: {condition: service_completed_successfully}`
here, which is compose's own "wait for exit 0 before starting me".

Each `assert_*` must evaluate true.
"""

import forge.workloads as fw

_api = fw.Workload {kind = "service", name = "api"}
_sync = fw.Workload {kind = "worker", name = "sync"}
_provision = fw.Workload {
    kind = "job"
    name = "provision-idp"
    command = ["/app/demo", "provision-idp"]
    env = {"IDP_URL" = "http://idp:8080"}
    before = ["api", "sync"]
}
_seed = fw.Workload {
    kind = "job"
    name = "seed"
    command = ["/app/demo", "seed"]
}

_components = [_api, _sync, _provision, _seed]

fragment = fw.compose_fragment(_components, "demo:latest")
_services = fragment.services

# --- Every Job becomes a top-level compose service ---
# Compose has no init-container concept, so unlike the k8s lowering a
# GATING job still renders a service; the ordering rides on the
# dependents' depends_on instead.
assert_gating_job_is_a_service = "provision-idp" in _services
assert_bare_job_is_a_service = "seed" in _services

# --- A one-shot must not be restarted ---
# Restarting a job that exited 0 re-gates every dependent waiting on
# service_completed_successfully.
assert_job_restart_no = _services["provision-idp"].restart == "no"
assert_seed_restart_no = _services["seed"].restart == "no"

# --- The job runs its own command and env ---
assert_job_command = _services["provision-idp"].command == ["/app/demo", "provision-idp"]
assert_job_env = _services["provision-idp"].environment["IDP_URL"] == "http://idp:8080"
assert_job_image = _services["provision-idp"].image == "demo:latest"

# --- Dependents wait for COMPLETION, not for start ---
# The long form is the only one that can carry a condition; the short
# list form means "started" and would give back the race.
_api_dep = _services["api"].depends_on
_sync_dep = _services["sync"].depends_on
assert_api_waits = _api_dep["provision-idp"].condition == "service_completed_successfully"
assert_sync_waits = _sync_dep["provision-idp"].condition == "service_completed_successfully"

# --- A job that gates nothing gates nothing ---
assert_seed_gates_nothing = "seed" not in _api_dep and "seed" not in _sync_dep

# --- A component with no gates gets no depends_on entry at all ---
# (Both workloads here ARE gated, so the fragment carries exactly two
# gated components and two jobs.)
assert_only_gated_get_depends_on = len([k for k in _services if "depends_on" in _services[k]]) == 2

# --- The single-component helper agrees with the fragment ---
# It takes the WORKLOAD, not its name: the broadcast selector gates by
# KIND, so a name alone cannot answer "is this gated?".
assert_helper_matches = fw.compose_depends_on(_api, _components) == _api_dep
assert_ungated_helper_empty = fw.compose_depends_on(_provision, _components) == {}

# --- BROADCAST: one declaration gates every workload, naming none ---
# The same wildcard the k8s lowering expands to init containers expands
# here to depends_on, so a migration declared once means the same thing
# whichever target the env renders to.
_migrate = fw.Workload {
    kind = "job"
    name = "migrate"
    command = ["/app/demo", "db", "migrate", "up"]
    before = [fw.BEFORE_ALL]
}
_broadcast_services = fw.compose_fragment([_api, _sync, _migrate, _seed], "demo:latest").services
assert_broadcast_gates_service = _broadcast_services["api"].depends_on["migrate"].condition == "service_completed_successfully"
assert_broadcast_gates_worker = _broadcast_services["sync"].depends_on["migrate"].condition == "service_completed_successfully"
# A broadcast job gates OTHER jobs too — a seed must not beat the
# migration that creates the table it seeds...
assert_broadcast_gates_other_jobs = _broadcast_services["seed"].depends_on["migrate"].condition == "service_completed_successfully"
# ...and the gated job keeps the service body it had (the depends_on is
# merged onto it, not substituted for it).
assert_gated_job_keeps_its_command = _broadcast_services["seed"].command == ["/app/demo", "seed"]
assert_gated_job_keeps_restart_no = _broadcast_services["seed"].restart == "no"
# ...but never itself: a job cannot wait for its own completion.
assert_broadcast_never_gates_itself = "depends_on" not in _broadcast_services["migrate"]
