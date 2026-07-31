"""
Positive test — the DEPLOY-TIME migration step.

A project that ships db/migrations/*.sql gets a `migrate` argv in
components_gen.json; the env's ComponentEnv carries it, and every
DEPLOYMENT-shaped workload renders it as an initContainer that runs
BEFORE the app container.

What this pins:

  1. The init container exists on server / worker / binary / operator,
     runs the SAME image as the app (the migrations are embedded IN the
     binary — there is no db/migrations directory in the runtime image),
     and carries the SAME env (so it reads the same DATABASE_URL from
     the same Secret).

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

  6. An EMPTY migrate command renders NO initContainers key at all, so a
     project without migrations is byte-identical to before this
     existed. A migration step running a command guaranteed to fail is
     worse than no step.
"""

import forge.components as fc

_MIGRATE = ["/app/demo", "db", "migrate", "up"]

_components = [
    fc.Server {
        name = "api"
        ports = [fc.ComponentPort {name = "http", port = 8080, expose = True}]
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
    fc.Worker {
        name = "reconciler"
        image = "demo"
        env_vars = [
            {name = "DATABASE_URL", secret_ref = "demo-secrets", secret_key = "database_url"}
        ]
    }
    fc.Binary {
        name = "ctl"
        image = "demo"
        command = ["/app/ctl"]
    }
    fc.Operator {
        name = "manager"
        image = "demo"
        crds = ["Widget"]
    }
    fc.Cron {
        name = "nightly"
        image = "demo"
        schedule = "@daily"
    }
]

_env = fc.ComponentEnv {
    namespace = "demo-prod"
    project = "demo"
    registry = "ghcr.io/acme"
    image_tag = "v1.2.3"
    network_policies = False
    migrate = _MIGRATE
}

manifests = fc.render_components(_components, _env)

_deployments = {m.metadata.name: m for m in manifests if m.kind == "Deployment"}
_api_pod = _deployments["api"].spec.template.spec
_api_init = _api_pod.initContainers[0]

# --- (1) present on every Deployment-shaped kind, same image, same env ---
assert_init_on_server = len(_deployments["api"].spec.template.spec.initContainers) == 1
assert_init_on_worker = len(_deployments["reconciler"].spec.template.spec.initContainers) == 1
assert_init_on_binary = len(_deployments["ctl"].spec.template.spec.initContainers) == 1
assert_init_on_operator = len(_deployments["manager"].spec.template.spec.initContainers) == 1

assert_init_name = _api_init.name == "migrate"
assert_init_command = _api_init.command == _MIGRATE
assert_init_image_matches_app = _api_init.image == _api_pod.containers[0].image
assert_init_env_matches_app = _api_init.env == _api_pod.containers[0].env

# The migration reads the DSN from the SAME Secret the app does — never a
# literal, in either container.
assert_init_db_from_secret = [e for e in _api_init.env if e.name == "DATABASE_URL"][0].valueFrom.secretKeyRef == {
    name = "demo-secrets"
    key = "database_url"
}

# --- (2) cron is excluded ---
_cronjob = [m for m in manifests if m.kind == "CronJob"][0]
assert_cron_has_no_init = "initContainers" not in _cronjob.spec.jobTemplate.spec.template.spec

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

# --- (6) no migrate command => no initContainers key at all ---
_env_no_migrate = fc.ComponentEnv {
    namespace = "demo-prod"
    project = "demo"
    registry = "ghcr.io/acme"
    image_tag = "v1.2.3"
    network_policies = False
}
_no_migrate_manifests = fc.render_components(_components, _env_no_migrate)
_no_migrate_pods = [
    m.spec.template.spec for m in _no_migrate_manifests if m.kind == "Deployment"
]
assert_no_init_without_migrate = len([p for p in _no_migrate_pods if "initContainers" in p]) == 0
assert_no_migrate_still_renders_workloads = len(_no_migrate_pods) == 4
