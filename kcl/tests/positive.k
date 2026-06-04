"""
Positive tests — each typed entity constructs cleanly and produces
the expected JSON shape.

These are not unit tests in the `kcl test` framework (which doesn't
exist yet at v0.11). Instead each test sets an `assert_*` identifier
to a boolean; the test runner (`tests/run.sh` or the Go-side test in
`internal/templates/`) checks every `assert_*` is `True` and that
`output` matches the expected JSON.
"""

import forge

# A Service with HostDeploy. env_vars carries KCL-declared per-env
# config; secrets_file points at a gitignored dotenv with API keys etc.
_svc_host = forge.Service {
    name = "admin-server"
    image = "myapp:dev"
    deploy = forge.HostDeploy {
        runner = "air"
        air_config = ".air.toml"
        env_vars = [
            forge.EnvVar { name = "LOG_LEVEL", value = "debug" }
        ]
        secrets_file = ".env.dev.secrets"
    }
}

# A Service with K8sCluster + Ingress.
_svc_cluster = forge.Service {
    name = "workspace-proxy"
    image = "myapp:dev"
    deploy = forge.K8sCluster {
        cluster = "k3d-myapp"
        namespace = "myapp-dev"
        registry = "localhost:5050"
        replicas = 2
        ports = [8080, 9090]
        ingress = forge.Ingress {host = "workspaces.example.com"}
    }
}

# A Service with BuildOnly + variants.
_svc_build = forge.Service {
    name = "reliant-daemon"
    deploy = forge.BuildOnly {
        build_variants = [
            forge.BuildVariant {name = "dev"}
            forge.BuildVariant {name = "prod", ldflags = ["-X main.Env=prod"]}
        ]
    }
}

# An Operator.
_op = forge.Operator {
    name = "workspace-controller"
    image = "myapp:dev"
    crds = ["Workspace"]
    cluster_rbac = forge.ClusterRBAC {rules = [{apiGroups = ["forge.io"], resources = ["workspaces"], verbs = ["*"]}]}
}

# A Frontend.
_fe = forge.Frontend {
    name = "admin-web"
    path = "frontends/admin-web"
}

# A scheduled CronJob.
_cron = forge.CronJob {
    name = "billing-sweeper"
    schedule = "@hourly"
    image = "myapp:prod"
    command = ["./myapp", "cron", "billing-sweep"]
}

# A one-shot Job (no schedule).
_job = forge.CronJob {
    name = "db-bootstrap"
    image = "myapp:prod"
    command = ["./myapp", "migrate", "up"]
}

_cm = forge.ConfigMap {
    name = "myapp-dev-config"
    data = {"LOG_LEVEL" = "info"}
}

_bundle = forge.Bundle {
    services = [_svc_host, _svc_cluster, _svc_build]
    operators = [_op]
    frontends = [_fe]
    cronjobs = [_cron, _job]
    config_maps = [_cm]
}

output = forge.render(_bundle)

# ── Invariants ─────────────────────────────────────────────────────

# Service.deploy discriminator dispatches correctly.
assert_host_discriminator = output.services[0].deploy.type == "host"
assert_cluster_discriminator = output.services[1].deploy.type == "cluster"
assert_buildonly_discriminator = output.services[2].deploy.type == "build-only"

# Missing optional fields project as null (not omitted).
assert_host_runner_present = output.services[0].deploy.runner == "air"
assert_host_env_vars_one = len(output.services[0].deploy.env_vars) == 1
assert_host_env_vars_name = output.services[0].deploy.env_vars[0].name == "LOG_LEVEL"
assert_host_secrets_file_set = output.services[0].deploy.secrets_file == ".env.dev.secrets"
assert_host_delve_port_default = output.services[0].deploy.delve_port == 2345
assert_cluster_resources_null = output.services[1].deploy.resources == None
assert_cluster_platform_null = output.services[1].deploy.platform == None

# K8sCluster.ingress projects with all keys, not just host.
assert_ingress_path_default = output.services[1].deploy.ingress.path == "/"
assert_ingress_tls_default = output.services[1].deploy.ingress.tls == True

# Buckets are present even when empty.
assert_bucket_services_count = len(output.services) == 3
assert_bucket_operators_count = len(output.operators) == 1
assert_bucket_frontends_count = len(output.frontends) == 1
assert_bucket_cronjobs_count = len(output.cronjobs) == 2

# CronJob with empty schedule projects as schedule: null.
assert_oneshot_schedule_null = output.cronjobs[1].schedule == None
assert_scheduled_schedule_set = output.cronjobs[0].schedule == "@hourly"

# Operator has its CRDs + cluster_rbac populated.
assert_operator_crds = output.operators[0].crds == ["Workspace"]
assert_operator_rbac_rules_count = len(output.operators[0].cluster_rbac.rules) == 1

# config_maps bucket projects cleanly.
assert_config_maps_count = len(output.config_maps) == 1
assert_config_map_data = output.config_maps[0].data["LOG_LEVEL"] == "info"
