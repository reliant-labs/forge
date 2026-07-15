"""
PROOF — target polymorphism: ONE agnostic `forge.Service` builder renders to
BOTH a host process (dev) AND a k8s Deployment (cluster), selected PER WORKLOAD
by the presence of a `host` block. See docs/design/FORGE_TARGET_POLYMORPHISM.md.

This is the acid test of the HOST adapter (`_service_to_host`) + the
per-workload target selector (`_project_workload`). We author ONE pure-agnostic
builder (`_admin_base`, modeled on control-plane's `admin_server_base()`: name /
image / command / env-map — ZERO target vocabulary) and place TWO instances of
it in ONE mixed Bundle:

  1. `_admin_host` — the SAME builder + a `host = HostOverrides{runner="air",
     air_config=…}` overlay. Its PRESENCE selects the host adapter: it projects
     to a `RenderedWorkload{deploy = HostDeploy}` (air, host env), and DROPS OUT of
     the k8s manifest stream.
  2. `_admin_k8s` — the SAME builder, plain (no `host` block). It renders
     EXACTLY as before: a normal k8s Deployment + Service.

We assert both render correctly, that the host workload's env is projected into
the HostDeploy block (where the Go host runner + JSON contract read it), and —
critically — that adding the host sibling does NOT perturb the k8s Deployment
(no host leakage: it is byte-identical to a pure-k8s Bundle). A third host
workload proves `command_override` (the sibling-repo `go run …` case).
"""

import forge

# ── ONE pure-agnostic builder — no target vocabulary ───────────────────────
# Modeled on control-plane's admin_server_base(): the env-invariant slice
# (name / image / command / env-map). Parameterized only by name so two
# instances can coexist in one Bundle (k8s object names must be unique).
_admin_base = lambda nm: str -> forge.Service {
    forge.Service {
        name = nm
        image = "control-plane"
        command = ["./control-plane"]
        args = ["server"]
        ports = [8090]
        env = {
            "APP_ENV" = {value = "dev"}
            "DATABASE_URL" = {value = "postgres://localhost:5434/cp"}
            "JWT_SECRET" = {from_secret = {name = "cp-secrets", key = "jwt"}}
        }
    }
}

# (1) HOST target — the SAME builder + a `host` block (presence => host adapter).
_admin_host = _admin_base("admin-server") | {
    host = forge.HostOverrides {
        runner = "air"
        air_config = ".air.admin-server.toml"
    }
}

# (2) k8s target — the SAME builder, plain (no host block => k8s adapter).
_admin_k8s = _admin_base("admin-worker")

# (3) HOST target with a command OVERRIDE — the sibling-repo `go run` case.
_reliant_host = _admin_base("reliant-api") | {
    host = forge.HostOverrides {
        runner = "go-run"
        working_dir = "../reliant"
        command_override = ["go", "run", "./cmd/reliant", "server", "api"]
    }
}

# ── The MIXED Bundle: 2 host + 1 k8s workloads, ONE workloads list ─────────
_ct = forge.ClusterTarget {
    cluster = "dev-cluster"
    namespace = "control-plane-dev"
    registry = "ghcr.io/reliant-labs"
}
_mixed = forge.Bundle {
    cluster_target = _ct
    workloads = [_admin_host, _admin_k8s, _reliant_host]
}

# ═══════════════════════════════════════════════════════════════════════════
# PROOF 1 — the HOST workload projects to a HostDeploy in the JSON contract.
# ═══════════════════════════════════════════════════════════════════════════
_json = forge.render(_mixed)
_svc_by_name = {s.name: s for s in _json.services}

# All three workloads appear in the JSON contract's `services` bucket.
assert_all_present = "admin-server" in _svc_by_name and "admin-worker" in _svc_by_name and "reliant-api" in _svc_by_name

_host_svc = _svc_by_name["admin-server"]
# host deploy discriminator + runner + air config.
assert_host_deploy_type = _host_svc.deploy.type == "host"
assert_host_runner = _host_svc.deploy.runner == "air"
assert_host_air_config = _host_svc.deploy.air_config == ".air.admin-server.toml"
# NO secrets_file — host secrets come from Bundle.secret_provider, not here.
assert_host_no_secrets_file = _host_svc.deploy.secrets_file == None
# command = the agnostic command+args, folded (no override on this one).
assert_host_command = _host_svc.command == ["./control-plane", "server"]

# env is projected into the HostDeploy BLOCK (where the Go host runner reads
# it — internal/cli/run.go hostEnvVarsToMap(host *HostDeploy)), NOT the
# top-level. This is the load-bearing correctness point.
_host_env = {e.name: e for e in _host_svc.deploy.env_vars}
assert_host_env_count = len(_host_svc.deploy.env_vars) == 3
assert_host_env_value = _host_env["APP_ENV"].value == "dev"
assert_host_env_value2 = _host_env["DATABASE_URL"].value == "postgres://localhost:5434/cp"
# the from_secret channel projects the same way the k8s adapter projects it.
assert_host_env_secret = _host_env["JWT_SECRET"].secret_ref == "cp-secrets" and _host_env["JWT_SECRET"].secret_key == "jwt"
# env follows author map-insertion order (so $(VAR) expansion is authorable).
assert_host_env_order = [e.name for e in _host_svc.deploy.env_vars] == ["APP_ENV", "DATABASE_URL", "JWT_SECRET"]
# top-level env_vars stays EMPTY for a host workload (legacy-faithful — env
# rides the deploy block).
assert_host_toplevel_env_empty = _host_svc.env_vars == []

# ── command_override wins over the agnostic command+args ───────────────────
_reliant_svc = _svc_by_name["reliant-api"]
assert_reliant_deploy_type = _reliant_svc.deploy.type == "host"
assert_reliant_runner = _reliant_svc.deploy.runner == "go-run"
assert_reliant_working_dir = _reliant_svc.deploy.working_dir == "../reliant"
assert_reliant_command_override = _reliant_svc.command == ["go", "run", "./cmd/reliant", "server", "api"]

# ── the k8s workload is a `cluster` deploy in the JSON contract ────────────
_k8s_svc = _svc_by_name["admin-worker"]
assert_k8s_deploy_type = _k8s_svc.deploy.type == "cluster"

# ═══════════════════════════════════════════════════════════════════════════
# PROOF 2 — in render_manifests, the k8s workload renders a Deployment and the
# host workloads DROP OUT (they are not `type == "cluster"`).
# ═══════════════════════════════════════════════════════════════════════════
_manifests = forge.render_manifests(_mixed, "latest", {}, False)
_deploys = {m.metadata.name: m for m in _manifests if m.kind == "Deployment"}

# ONLY the k8s workload produced a Deployment.
assert_k8s_deploy_exists = "admin-worker" in _deploys
assert_host_no_deploy = "admin-server" not in _deploys
assert_reliant_no_deploy = "reliant-api" not in _deploys
assert_exactly_one_deploy = len(_deploys) == 1

_k8s_dep = _deploys["admin-worker"]
_k8s_ctr = _k8s_dep.spec.template.spec.containers[0]
assert_k8s_image = _k8s_ctr.image == "ghcr.io/reliant-labs/control-plane:latest"
assert_k8s_command = _k8s_ctr.command == ["./control-plane", "server"]
_k8s_env = {e.name: e for e in _k8s_ctr.env}
assert_k8s_env_value = _k8s_env["APP_ENV"].value == "dev"
assert_k8s_env_secret = _k8s_env["JWT_SECRET"].valueFrom.secretKeyRef == {name = "cp-secrets", key = "jwt"}

# ═══════════════════════════════════════════════════════════════════════════
# PROOF 3 — NO HOST LEAKAGE: the k8s Deployment is byte-identical whether or
# not the host siblings share the Bundle. The plain workload renders EXACTLY
# as a pure-k8s Bundle would.
# ═══════════════════════════════════════════════════════════════════════════
_pure = forge.Bundle {cluster_target = _ct, workloads = [_admin_k8s]}
_pure_manifests = forge.render_manifests(_pure, "latest", {}, False)
_pure_dep = [m for m in _pure_manifests if m.kind == "Deployment" and m.metadata.name == "admin-worker"][0]
assert_no_host_leakage = _k8s_dep == _pure_dep

# ═══════════════════════════════════════════════════════════════════════════
# VERDICT:
#   ONE builder (`_admin_base`) → TWO targets, chosen OFF the builder by the
#   presence of a `host` block, never baked in. The host workload renders to a
#   HostDeploy (air, host env in the deploy block); the same builder plain
#   renders a normal k8s Deployment, byte-identical to a pure-k8s Bundle. This
#   is exactly the shared-builder break (docs/design/CONTROL_PLANE_MIGRATION.md B3) unwound.
# ═══════════════════════════════════════════════════════════════════════════
