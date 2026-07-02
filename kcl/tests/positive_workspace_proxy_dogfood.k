"""
DOGFOOD — a REAL control-plane service expressed in the clean agnostic model.

This is the proof that the extended, target-agnostic `forge.Service` can
faithfully express an actual production workload: control-plane's PROD
`workspace-proxy` (modeled on deploy/kcl/prod/main.k `_proxy_env` + the proxy
service block). Nothing here touches control-plane — it is a forge fixture that
re-expresses that service in the clean vocabulary and asserts the k8s adapter
renders it correctly.

What it exercises (the extended Service surface):
  * a project `schema WorkspaceProxy(forge.Service)` with defaults (the
    always-present identity/env), a `workspace_base`, and a per-env
    `workspace_proxy_prod = workspace_base | { …prod deltas… }` built with
    native map-merge on the env (`base.env | { … }`) — no k8s vocabulary;
  * env across ALL THREE agnostic channels — value / from_secret / from_config
    — plus a whole-configmap `env_from` bulk-import;
  * `resources` (the prod proxy scale floor);
  * `volumes` — the daemon-cluster kubeconfig Secret mounted read-only (the
    real prod cross-cluster mount);
  * the TIERED k8s escape hatch `Service.k8s` (K8sOverrides): a Tier-1 typed
    knob (`annotations`) AND the Tier-2 floor (`raw_manifests` = the real GKE
    HealthCheckPolicy the prod proxy ships as `extra_manifests`).

Then it renders through `Bundle.workloads` (the agnostic authoring entry, k8s
adapter runs INSIDE render_manifests) and asserts the Deployment + Service are
correct: image, port, env across channels, envFrom, resources, the mounted
Secret volume, and both escape-hatch tiers.
"""

import forge

# ── The project's own typed workload schema, inheriting the agnostic core ──
# Carries the defaults that are TRUE for every workspace-proxy env (identity,
# the always-present env vars, the listen port). Per-env files overlay the rest.
schema WorkspaceProxy(forge.Service):
    name: str = "workspace-proxy"
    # The control-plane multi-service binary; the proxy is a cobra subcommand.
    image: str = "control-plane"
    command: [str] = ["/app/control-plane"]
    args: [str] = ["workspace-proxy"]
    ports: [int] = [8080]
    # Env every workspace-proxy carries regardless of env. Per-env deltas merge
    # onto this with native map-merge (`workspace_base.env | { … }`).
    env: {str: forge.EnvSource} = {
        "OTEL_SERVICE_NAME" = {value = "workspace-proxy"}
        "PORT" = {value = "8080"}
        # The secretless-proxy PDP token signing secret — a from_secret channel
        # present in every env (control-plane-internal/secret).
        "INTERNAL_SERVICE_SECRET" = {from_secret = {name = "control-plane-internal", key = "secret"}}
        # Log level read from the forge-generated ConfigMap — a from_config channel.
        "LOG_LEVEL" = {from_config = {name = "control-plane-config", key = "log-level"}}
    }

# The portable base instance (defaults only + the dev-scale floor).
workspace_base = WorkspaceProxy {
    replicas = 1
    resources = forge.Resources {
        cpu_request_millicores = forge.cpu(0.1)    # 100m
        cpu_limit_millicores = forge.cpu(0.5)      # 500m
        memory_request_bytes = forge.mib(128)      # 128Mi
        memory_limit_bytes = forge.mib(256)        # 256Mi
    }
}

# ── PROD: base | per-env deltas ────────────────────────────────────────────
# The real GKE backend health-check override the prod proxy ships as an
# extra_manifest (lib/infra.healthcheck_policy) — the Tier-2 raw-manifest floor.
_healthcheck_policy = {
    apiVersion = "networking.gke.io/v1"
    kind = "HealthCheckPolicy"
    metadata = {name = "workspace-proxy"}
    spec = {
        default = {config = {type = "HTTP", httpHealthCheck = {requestPath = "/healthz"}}}
        targetRef = {group = "", kind = "Service", name = "workspace-proxy"}
    }
}

workspace_proxy_prod = workspace_base | {
    replicas = 2
    resources = forge.Resources {
        cpu_request_millicores = forge.cpu(0.25)   # 250m
        cpu_limit_millicores = forge.cpu(1)        # 1
        memory_request_bytes = forge.mib(256)      # 256Mi
        memory_limit_bytes = forge.mib(512)        # 512Mi
    }
    # Native map-merge: keep the base channels, add the prod-only deltas.
    env = workspace_base.env | {
        "PROXY_BASE_DOMAIN" = {value = "workspaces.reliantapi.com"}
        "APP_URL" = {value = "https://app.reliantlabs.io"}
        # secretless-proxy / RPC authz inversion (PROD ONLY).
        "PROXY_AUTHZ_MODE" = {value = "rpc"}
        "PROXY_AUTHZ_URL" = {value = "http://admin-server.control-plane-prod.svc.cluster.local:8090"}
        # daemon-cluster isolation (PROD ONLY).
        "DAEMON_CLUSTER_ID" = {value = "prod-daemon"}
    }
    # Whole-configmap bulk import (k8s envFrom).
    env_from = [forge.EnvFrom {config = "control-plane-proxy-extra"}]
    # The prod cross-cluster mount: the prod-daemon kubeconfig Secret, read-only.
    volumes = [
        forge.Volume {
            name = "daemon-cluster-kubeconfig"
            mount_path = "/etc/daemon-cluster"
            type = "secret"
            source = "cp-daemon-kubeconfig"
            read_only = True
        }
    ]
    # The TIERED k8s escape hatch.
    k8s = forge.K8sOverrides {
        # Tier 1 — a typed, discoverable pod annotation.
        annotations = {"reliant.dev/scrape" = "true"}
        # Tier 2 — the ultimate floor: the verbatim GKE HealthCheckPolicy.
        raw_manifests = [_healthcheck_policy]
    }
}

# ── Render through Bundle.workloads (adapter runs INTERNALLY) ───────────────
_prod = forge.ClusterTarget {
    cluster = "control-plane-prod-cluster"
    namespace = "control-plane-prod"
    registry = "ghcr.io/reliant-labs"
    platform = "amd64"
}

_bundle = forge.Bundle {
    cluster_target = _prod
    workloads = [workspace_proxy_prod]
}
manifests = forge.render_manifests(_bundle, "v1.4.2", {}, False)

_dep = [m for m in manifests if m.kind == "Deployment" and m.metadata.name == "workspace-proxy"][0]
_svc = [m for m in manifests if m.kind == "Service" and m.metadata.name == "workspace-proxy"][0]
_pod = _dep.spec.template.spec
_ctr = _pod.containers[0]

# ── image, command, replicas, resources ────────────────────────────────────
assert_image = _ctr.image == "ghcr.io/reliant-labs/control-plane:v1.4.2"
assert_command = _ctr.command == ["/app/control-plane", "workspace-proxy"]
assert_replicas = _dep.spec.replicas == 2
assert_res_req = _ctr.resources.requests == {cpu = "250m", memory = "256Mi"}
assert_res_lim = _ctr.resources.limits == {cpu = "1", memory = "512Mi"}

# ── port ───────────────────────────────────────────────────────────────────
assert_container_port = _ctr.ports[0].containerPort == 8080
assert_svc_port = _svc.spec.ports[0].port == 8080 and _svc.spec.ports[0].targetPort == 8080

# ── env across all three channels (base + prod deltas) ─────────────────────
_env = {e.name: e for e in _ctr.env}
# value channel — base + prod.
assert_env_base_value = _env["OTEL_SERVICE_NAME"].value == "workspace-proxy"
assert_env_port_value = _env["PORT"].value == "8080"
assert_env_prod_value = _env["PROXY_AUTHZ_MODE"].value == "rpc"
assert_env_domain = _env["PROXY_BASE_DOMAIN"].value == "workspaces.reliantapi.com"
# from_secret -> secretKeyRef.
assert_env_secret = _env["INTERNAL_SERVICE_SECRET"].valueFrom.secretKeyRef == {name = "control-plane-internal", key = "secret"}
# from_config -> configMapKeyRef.
assert_env_config = _env["LOG_LEVEL"].valueFrom.configMapKeyRef == {name = "control-plane-config", key = "log-level"}
# The base channels survived the `base.env | { deltas }` merge
# (4 base: OTEL_SERVICE_NAME/PORT/INTERNAL_SERVICE_SECRET/LOG_LEVEL +
#  5 prod deltas: PROXY_BASE_DOMAIN/APP_URL/PROXY_AUTHZ_MODE/PROXY_AUTHZ_URL/
#  DAEMON_CLUSTER_ID).
assert_env_count = len(_ctr.env) == 9

# ── env_from -> envFrom (whole-configmap bulk import) ──────────────────────
assert_env_from = _ctr.envFrom == [{configMapRef = {name = "control-plane-proxy-extra"}}]

# ── volumes: the daemon-cluster kubeconfig Secret, read-only ───────────────
_vol = [v for v in _pod.volumes if v.name == "daemon-cluster-kubeconfig"][0]
assert_vol_secret = _vol.secret.secretName == "cp-daemon-kubeconfig"
_mount = [m for m in _ctr.volumeMounts if m.name == "daemon-cluster-kubeconfig"][0]
assert_mount_path = _mount.mountPath == "/etc/daemon-cluster"
assert_mount_ro = _mount.readOnly == True
# The restricted-PSA baseline is untouched.
assert_ro_rootfs = _ctr.securityContext.readOnlyRootFilesystem == True

# ── k8s escape hatch, Tier 1: typed annotation on the pod template ─────────
assert_pod_annotation = _dep.spec.template.metadata.annotations == {"reliant.dev/scrape" = "true"}

# ── k8s escape hatch, Tier 2: raw_manifests owned + label-stamped + routed ─
_hcp = [m for m in manifests if m.kind == "HealthCheckPolicy"][0]
assert_hcp_present = _hcp.spec.targetRef.name == "workspace-proxy"
# Rendered verbatim EXCEPT the owner label the deploy layer routes by.
assert_hcp_owner_label = _hcp.metadata.labels["app.kubernetes.io/name"] == "workspace-proxy"
