"""
Positive test — the DEPLOY-TIME MIGRATION STEP, as an ordinary Job.

Migrate used to be a special case: a `migrate: [str]` argv on the
WorkloadEnv, which the render layer knew to lower as an initContainer on
every Deployment-shaped workload. It is now a plain `kind = "job"`
Workload with `before = [BEFORE_ALL]` — one instance of a general
primitive rather than the single deploy-time ordering forge could
express.

THIS FILE IS THE NO-REGRESSION PROOF. Every property the bespoke step
guaranteed is re-asserted here against the Job-based render, because the
point of the move is that nothing about the SAFETY changed — only where
the declaration lives.

What this pins:

  1. The init container exists on service / worker / operator, runs the
     dependent's image (the migrations are embedded IN the binary — there
     is no db/migrations directory in the runtime image), and reads its
     DSN from the same Secret the app does.

  2. `cron` is EXCLUDED. A CronJob pod fires on a schedule, not as part
     of a release rollout; an init container there would re-run the
     migrator on every tick to no purpose.

  3. The init container satisfies the restricted Pod Security Standard
     (non-root, no privilege escalation, all caps dropped, read-only
     rootfs) — a pod in a `restricted` namespace is REJECTED otherwise,
     and the app container next to it already complies.

  4. It requests the DEFAULT small resource block, not the workload's
     per-env block. k8s computes a pod's effective request as
     max(largest init request, sum of container requests), so a 2-CPU
     app request inherited by a one-second init would inflate scheduling
     for every replica.

  5. Env entries in the init container obey the same EXACTLY-ONE-SOURCE
     rule as the app container. `value` + `valueFrom` on one entry is
     rejected by the API SERVER only ("may not be specified when `value`
     is not empty") — client-side dry-run waves it through.

  6. AN ENVIRONMENT THAT DOES NOT DEPLOY THE MIGRATE WORKLOAD RENDERS NO
     INIT CONTAINER AT ALL. This is the successor to `migrate = []`: the
     honest answer for an env that migrates out of band, and the same
     gesture as dropping any other workload an env does not run.

  7. ADDING A WORKLOAD DOES NOT SILENTLY SKIP MIGRATION. This is the
     property the enumerated form could not hold and the whole reason
     the broadcast selector exists — see the last section.
"""

import forge.workloads as fw

_MIGRATE = ["/app/demo", "db", "migrate", "up"]

# The migration step, declared like anything else the project ships.
_migrate = fw.Workload {
    kind = "job"
    name = "migrate"
    command = _MIGRATE
    image = "demo"
    before = [fw.BEFORE_ALL]
    env_vars = [
        {name = "DATABASE_URL", secret_ref = "demo-secrets", secret_key = "database_url"}
    ]
}

_workloads = [
    fw.Workload {
        kind = "service"
        name = "api"
        ports = [fw.Port {name = "http", port = 8080, expose = True}]
        image = "demo"
        replicas = 3
        resources = {
            cpu_request_millicores = 2000
            cpu_limit_millicores = 4000
            memory_request_bytes = 2147483648
            memory_limit_bytes = 4294967296
        }
        env = {LOG_LEVEL = "info"}
        env_vars = [
            {name = "DATABASE_URL", secret_ref = "demo-secrets", secret_key = "database_url"}
        ]
    }
    fw.Workload {
        kind = "worker"
        name = "reconciler"
        image = "demo"
        env_vars = [
            {name = "DATABASE_URL", secret_ref = "demo-secrets", secret_key = "database_url"}
        ]
    }
    fw.Workload {
        kind = "worker"
        name = "ctl"
        image = "demo"
        command = ["/app/ctl"]
    }
    fw.Workload {
        kind = "operator"
        name = "manager"
        image = "demo"
        crds = ["Widget"]
    }
    fw.Workload {
        kind = "cron"
        name = "nightly"
        image = "demo"
        schedule = "@daily"
    }
]

_env = fw.WorkloadEnv {
    namespace = "demo-prod"
    project = "demo"
    registry = "ghcr.io/acme"
    image_tag = "v1.2.3"
    network_policies = False
}

manifests = fw.render_workloads(_workloads + [_migrate], _env)

_deployments = {m.metadata.name: m for m in manifests if m.kind == "Deployment"}
_api_pod = _deployments["api"].spec.template.spec
_api_init = _api_pod.initContainers[0]

# --- (1) present on every Deployment-shaped kind, same image, right env ---
assert_init_on_service = len(_deployments["api"].spec.template.spec.initContainers) == 1
assert_init_on_worker = len(_deployments["reconciler"].spec.template.spec.initContainers) == 1
assert_init_on_custom_entrypoint = len(_deployments["ctl"].spec.template.spec.initContainers) == 1
assert_init_on_operator = len(_deployments["manager"].spec.template.spec.initContainers) == 1

assert_init_name = _api_init.name == "migrate"
assert_init_command = _api_init.command == _MIGRATE
assert_init_image_matches_app = _api_init.image == _api_pod.containers[0].image

# A workload that declares NO database env still gets the migration step,
# and the step still carries its own DSN. Under the old bespoke field the
# init container inherited the DEPENDENT's env, so this pod's migration
# ran with no DATABASE_URL at all; as a job it declares what it needs.
_DSN_REF = {name = "demo-secrets", key = "database_url"}
_ctl_init = _deployments["ctl"].spec.template.spec.initContainers[0]
_ctl_dsn = [e for e in _ctl_init.env if e.name == "DATABASE_URL"][0]
assert_migrate_carries_own_dsn_everywhere = _ctl_dsn.valueFrom.secretKeyRef == _DSN_REF

# The migration reads the DSN from the SAME Secret the app does — never a
# literal, in either container.
_api_dsn = [e for e in _api_init.env if e.name == "DATABASE_URL"][0]
assert_init_db_from_secret = _api_dsn.valueFrom.secretKeyRef == _DSN_REF

# --- (2) cron is excluded ---
_cronjob = [m for m in manifests if m.kind == "CronJob"][0]
assert_cron_has_no_init = "initContainers" not in _cronjob.spec.jobTemplate.spec.template.spec

# --- The broadcast job renders no standalone Job of its own ---
# Rendering one would run the migration twice: once ordered as an init
# container, once unordered as a Job racing the very pods it gates.
assert_no_standalone_migrate_job = len([m for m in manifests if m.kind == "Job"]) == 0

# --- (3) restricted-PSA compliant, exactly like the app container ---
assert_init_non_root = _api_init.securityContext.runAsNonRoot == True
assert_init_no_privilege_escalation = _api_init.securityContext.allowPrivilegeEscalation == False
assert_init_drops_all_caps = _api_init.securityContext.capabilities.drop == ["ALL"]
assert_init_readonly_rootfs = _api_init.securityContext.readOnlyRootFilesystem == True

# --- (4) default resources, NOT the workload's inflated per-env block ---
assert_init_default_cpu = _api_init.resources.requests.cpu == "100m"
assert_init_default_memory = _api_init.resources.requests.memory == "128Mi"
assert_app_keeps_its_own_resources = _api_pod.containers[0].resources.requests.cpu == "2"

# --- (5) exactly one source per env entry (server-side-only rejection) ---
_dual = [e for e in _api_init.env if "value" in e and "valueFrom" in e]
assert_init_never_two_sources = len(_dual) == 0

# --- (6) an env that does not deploy migrate gets no init container ---
# The successor to `migrate = []`. An env that applies migrations out of
# band omits the workload from the list it deploys.
_no_migrate_manifests = fw.render_workloads(_workloads, _env)
_no_migrate_pods = [
    m.spec.template.spec for m in _no_migrate_manifests if m.kind == "Deployment"
]
assert_no_init_without_migrate = len([p for p in _no_migrate_pods if "initContainers" in p]) == 0
assert_no_migrate_still_renders_workloads = len(_no_migrate_pods) == 4

# ---------------------------------------------------------------------------
# (7) ADDING A WORKLOAD DOES NOT SILENTLY SKIP MIGRATION
# ---------------------------------------------------------------------------
# This is the property an ENUMERATED `before` cannot hold, and the reason
# the broadcast selector exists. `before = ["api", "reconciler", ...]`
# would still render, still pass every check, and still gate the four
# workloads it names — while the newly added fifth serves traffic against
# a schema it does not have. The failure is silent at render time and
# surfaces as a runtime error in a message that names none of this.
#
# Note what is NOT written below: the env's declaration is unchanged. The
# ONLY edit is appending a workload — exactly what a developer does six
# months from now — and it is gated with no list to remember to update.
_late_addition = fw.Workload {
    kind = "service"
    name = "billing"
    image = "demo"
    ports = [fw.Port {name = "http", port = 8080, expose = True}]
}
_grown = fw.render_workloads(_workloads + [_migrate, _late_addition], _env)
_billing_pod = [
    m.spec.template.spec for m in _grown
    if m.kind == "Deployment" and m.metadata.name == "billing"
][0]
assert_new_workload_is_gated = [c.name for c in _billing_pod.initContainers] == ["migrate"]

# ...and every workload that was already there is still gated, so the
# addition did not perturb the rest.
_grown_deployments = [m for m in _grown if m.kind == "Deployment"]
assert_every_deployment_gated = len([
    d for d in _grown_deployments
    if "migrate" in [c.name for c in d.spec.template.spec.initContainers]
]) == len(_grown_deployments)
