"""
Positive test — the agnostic-core collapse: ONE user-facing agnostic
`forge.Service` placed in `Bundle.workloads`, projected onto the demoted
k8s-shaped `forge.K8sWorkload` INSIDE `forge.render_manifests` (the k8s
adapter runs in the render pipeline; the user NEVER calls it), producing a
correct Deployment + Service.

Pins the load-bearing claims of the collapse (FORGE_KCL_IDIOMS.md §2 / §7):

  1. ONE realistic agnostic Service (modeled on control-plane prod
     `workspace-proxy`: image, port 8080, env across ALL THREE EnvSource
     channels — value / from_secret / from_config — plus a whole-secret
     `env_from` bulk-import) authored with ZERO k8s vocabulary.
  2. Placed in Bundle.workloads and rendered through the UNCHANGED
     render_manifests, the internal adapter yields a correct Deployment +
     Service:
       * container image resolved (registry + tag),
       * command (command+args fold), replicas, resources mapped,
       * containerPort + Service port correct,
       * env: value / secretKeyRef / configMapKeyRef all present + correct,
       * env_from -> k8s envFrom (secretRef).
"""

import forge

# ── (1) ONE realistic agnostic Service — no k8s vocabulary ─────────
_workspace_proxy = forge.Service {
    name = "workspace-proxy"
    image = "control-plane"
    command = ["./workspace_proxy"]
    args = ["--serve"]
    ports = [8080]
    replicas = 2
    resources = forge.Resources {
        cpu_request_millicores = forge.cpu(0.25)   # 250m
        cpu_limit_millicores = forge.cpu(1)        # 1
        memory_request_bytes = forge.mib(256)      # 256Mi
        memory_limit_bytes = forge.mib(512)        # 512Mi
    }
    env = {
        "OTEL_SERVICE_NAME" = {value = "workspace-proxy"}
        "APP_URL" = {value = "https://app.reliantlabs.io"}
        "NATS_PASSWORD" = {from_secret = {name = "control-plane-nats", key = "password"}}
        "LOG_LEVEL" = {from_config = {name = "control-plane-config", key = "log-level"}}
    }
    # Whole-secret bulk-import -> k8s envFrom.
    env_from = [forge.EnvFrom {secret = "control-plane-shared"}]
}

# ── (2) put it in the Bundle and render — adapter runs INTERNALLY ───
# Env-wide k8s coordinates (the SSOT), stated once.
_prod = forge.ClusterTarget {
    cluster = "prod-cluster"
    namespace = "control-plane-prod"
    registry = "ghcr.io/reliant-labs"
}

_bundle = forge.Bundle {cluster_target = _prod, workloads = [_workspace_proxy]}
manifests = forge.render_manifests(_bundle, "latest", {}, False)

_deploys = {m.metadata.name: m for m in manifests if m.kind == "Deployment"}
_svcs = {m.metadata.name: m for m in manifests if m.kind == "Service"}

# ── Deployment: identity, image, command, replicas, resources ──────
assert_deploy_exists = "workspace-proxy" in _deploys
_dep = _deploys["workspace-proxy"]
_container = _dep.spec.template.spec.containers[0]

assert_image = _container.image == "ghcr.io/reliant-labs/control-plane:latest"
# command + args folded into the single argv.
assert_command = _container.command == ["./workspace_proxy", "--serve"]
assert_replicas = _dep.spec.replicas == 2
assert_resources_req = _container.resources.requests == {cpu = "250m", memory = "256Mi"}
assert_resources_lim = _container.resources.limits == {cpu = "1", memory = "512Mi"}

# ── Deployment: containerPort ──────────────────────────────────────
assert_container_port = _container.ports[0].containerPort == 8080

# ── Deployment: env across all three channels ──────────────────────
_env_by_name = {e.name: e for e in _container.env}
assert_env_count = len(_container.env) == 4

# value channel
assert_env_value = _env_by_name["OTEL_SERVICE_NAME"].value == "workspace-proxy"
assert_env_value2 = _env_by_name["APP_URL"].value == "https://app.reliantlabs.io"
# from_secret -> secretKeyRef
assert_env_secret = _env_by_name["NATS_PASSWORD"].valueFrom.secretKeyRef == {name = "control-plane-nats", key = "password"}
# from_config -> configMapKeyRef
assert_env_config = _env_by_name["LOG_LEVEL"].valueFrom.configMapKeyRef == {name = "control-plane-config", key = "log-level"}
# env follows the author's map-literal INSERTION order (not sorted) — so
# `$(VAR)` dependent expansion is authorable.
assert_env_order = [e.name for e in _container.env] == ["OTEL_SERVICE_NAME", "APP_URL", "NATS_PASSWORD", "LOG_LEVEL"]

# ── Deployment: env_from -> envFrom (whole-secret bulk import) ──────
assert_env_from = _container.envFrom == [{secretRef = {name = "control-plane-shared"}}]

# ── Service: port projection ───────────────────────────────────────
assert_svc_exists = "workspace-proxy" in _svcs
_svc_ports = _svcs["workspace-proxy"].spec.ports
assert_svc_port = _svc_ports[0].port == 8080 and _svc_ports[0].targetPort == 8080
