"""
ACCEPTANCE TEST for the agnostic-core collapse — the schema-per-service user
pattern (the canonical KCL "app model + per-target render" abstraction).

Proves end-to-end that a project author works ENTIRELY in the agnostic
`forge.Service` surface:

  1. INHERITANCE — a per-service schema `WorkspaceProxy(forge.Service)`
     declared in a DIFFERENT package than `forge` (this `forge_tests` module
     consumes `forge` as an external `-E forge=` dep, exactly like a real
     user's `deploy/kcl/...`), fixing its own defaults. A `WorkspaceProxy`
     instance satisfies `forge.Service`.
  2. A base instance `workspace_base = WorkspaceProxy {}`.
  3. PER-ENV OVERRIDE that MERGES into env — the load-bearing idiom:
        workspace_proxy_prod = workspace_base | { env: {"APP_URL" = ...} }
     The COLON (`env:`) unions the override INTO the base env, so every base
     key SURVIVES and APP_URL is ADDED. (An `=` there would DESTROY the base
     env — see negative note below; proven empirically before writing this.)
  4. RENDER through the pipeline via `Bundle.workloads` — the k8s adapter runs
     INTERNALLY inside `render_manifests`; the user never calls it.

Cross-package identity note
===========================
`_service_to_k8s` is typed `lambda svc: Service -> ...` where its `Service` is
`forge.Service` resolved INSIDE the forge module. A `WorkspaceProxy` built
here in `forge_tests` — whose base `forge.Service` resolves via the external
`-E forge=` binding — satisfies that parameter because BOTH sides resolve the
schema by the MODULE NAME `forge`, so the type identity is SHARED (no fork).
"""

import forge

# ── (1) The USER MODEL: a per-service schema inheriting the agnostic
#    core, setting its own defaults. Declared in `forge_tests` — a
#    DIFFERENT package than `forge`. ──────────────────────────────────
schema WorkspaceProxy(forge.Service):
    name = "workspace-proxy"
    image = "control-plane"
    ports = [8080]
    env = {
        "OTEL_SERVICE_NAME" = {value = "workspace-proxy"}
        "LEADER_ELECTION" = {value = "true"}
    }
    # A whole-secret bulk-import carried on the base schema.
    env_from = [forge.EnvFrom {secret = "control-plane-shared"}]

# ── (2) base instance — zero-arg construction on inherited defaults ──
workspace_base = WorkspaceProxy {}

assert_base_is_service = typeof(workspace_base) == "WorkspaceProxy"
assert_base_name = workspace_base.name == "workspace-proxy"
assert_base_env_keys = sorted([k for k in workspace_base.env]) == ["LEADER_ELECTION", "OTEL_SERVICE_NAME"]

# ── (3) per-env override — the MERGE idiom (colon unions into env) ───
# `env:` (colon) merges the override INTO the base env; base keys survive.
workspace_proxy_prod = workspace_base | {
    env: {"APP_URL" = {value = "https://app.reliantlabs.io"}}
}

# Base env keys SURVIVE the prod override, AND APP_URL is added.
assert_prod_env_keys = sorted([k for k in workspace_proxy_prod.env]) == \
    ["APP_URL", "LEADER_ELECTION", "OTEL_SERVICE_NAME"]
assert_prod_app_url = workspace_proxy_prod.env["APP_URL"].value == "https://app.reliantlabs.io"
assert_prod_leader_survived = workspace_proxy_prod.env["LEADER_ELECTION"].value == "true"
assert_prod_otel_survived = workspace_proxy_prod.env["OTEL_SERVICE_NAME"].value == "workspace-proxy"
# The base is UNMUTATED by the override (immutable value semantics).
assert_base_unchanged = "APP_URL" not in workspace_base.env

# ── (4) render the prod workload through the pipeline ────────────────
_prod = forge.ClusterTarget {
    cluster = "prod-cluster"
    namespace = "control-plane-prod"
    registry = "ghcr.io/reliant-labs"
}

_bundle = forge.Bundle {cluster_target = _prod, workloads = [workspace_proxy_prod]}
manifests = forge.render_manifests(_bundle, "latest", {}, False)

_deploys = {m.metadata.name: m for m in manifests if m.kind == "Deployment"}
_svcs = {m.metadata.name: m for m in manifests if m.kind == "Service"}

assert_deploy_exists = "workspace-proxy" in _deploys
assert_svc_exists = "workspace-proxy" in _svcs

_container = _deploys["workspace-proxy"].spec.template.spec.containers[0]
assert_image = _container.image == "ghcr.io/reliant-labs/control-plane:latest"
assert_container_port = _container.ports[0].containerPort == 8080
assert_replicas = _deploys["workspace-proxy"].spec.replicas == 1

# All three env keys render (sorted by name by env_project).
_env_by_name = {e.name: e for e in _container.env}
assert_env_count = len(_container.env) == 3
assert_env_app_url = _env_by_name["APP_URL"].value == "https://app.reliantlabs.io"
assert_env_otel = _env_by_name["OTEL_SERVICE_NAME"].value == "workspace-proxy"
assert_env_leader = _env_by_name["LEADER_ELECTION"].value == "true"

# env_from bulk-import projects to k8s envFrom.
assert_env_from = _container.envFrom == [{secretRef = {name = "control-plane-shared"}}]

# Service port projection.
assert_svc_port = _svcs["workspace-proxy"].spec.ports[0].port == 8080
