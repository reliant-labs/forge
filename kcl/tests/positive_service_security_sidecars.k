"""
Positive test — the agnostic `forge.Service` extensions that let workloads
which USED to be raw Deployments fold into the model:

  1. `security` (SecurityOverrides) — pod/container securityContext DELTAS over
     forge's restricted-PSA default. Proves a NATS-like service (needs
     `fs_group` to write JetStream + a non-65532 UID) is now an agnostic
     Service, and that each set field OVERRIDES the default while unset fields
     keep it.
  2. `sidecars` + `init` ([Container]) — a service with an OTel-collector
     sidecar and a schema-migration init container renders the extra pod
     containers (image resolved, PSA container SC, env projected), sidecars
     AFTER the primary and init as initContainers.
  2b. `health` (HealthProbe) — one probe config renders as BOTH livenessProbe
     AND readinessProbe (k8s) + a compose `healthcheck`; http_path->httpGet,
     tcp->tcpSocket, port defaults to the first declared port.
  3. HAPPY PATH UNCHANGED — a plain single-container Service with NO security /
     sidecars / init / health renders BYTE-IDENTICALLY to a hand-authored
     RenderedWorkload baseline (containers == [primary], no initContainers, no
     probes, default SC).
"""

import forge

_target = forge.ClusterTarget {
    cluster = "infra-cluster"
    namespace = "control-plane-dev"
    registry = "ghcr.io/reliant-labs"
}

# ── (1) NATS-like service: security override (fsGroup + UID 1000) ──────────
# This is the deploy/kcl/lib/infra.k `nats_dev` posture expressed agnostically:
# runAsUser/runAsGroup=1000 + fsGroup=1000 so JetStream can write /data.
_nats = forge.Service {
    name = "nats"
    image = "nats:2.10.24-alpine"
    command = ["nats-server"]
    args = ["-js", "-sd", "/data", "-m", "8222"]
    ports = [4222, 8222]
    security = forge.SecurityOverrides {
        run_as_user = 1000
        run_as_group = 1000
        fs_group = 1000
    }
    volumes = [
        forge.Volume {name = "data", mount_path = "/data"}   # empty_dir scratch
    ]
}

_nats_manifests = forge.render_manifests(forge.Bundle {cluster_target = _target, workloads = [_nats]}, "latest", {}, False)
_nats_dep = [m for m in _nats_manifests if m.kind == "Deployment" and m.metadata.name == "nats"][0]
_nats_pod = _nats_dep.spec.template.spec
_nats_ctr = _nats_pod.containers[0]

# pod-level securityContext: fsGroup + UID/GID overridden, seccomp default kept.
assert_pod_fsgroup = _nats_pod.securityContext.fsGroup == 1000
assert_pod_runasuser = _nats_pod.securityContext.runAsUser == 1000
assert_pod_runasgroup = _nats_pod.securityContext.runAsGroup == 1000
assert_pod_seccomp_default = _nats_pod.securityContext.seccompProfile == {type = "RuntimeDefault"}
# runAsNonRoot NOT overridden => keeps the restricted default True.
assert_pod_nonroot_default = _nats_pod.securityContext.runAsNonRoot == True
# container-level: UID must ALSO be overridden (container wins in k8s), while
# the unset knobs keep the restricted default.
assert_ctr_runasuser = _nats_ctr.securityContext.runAsUser == 1000
assert_ctr_rootfs_default = _nats_ctr.securityContext.readOnlyRootFilesystem == True
assert_ctr_caps_default = _nats_ctr.securityContext.capabilities == {drop = ["ALL"]}
assert_ctr_no_privileged = "privileged" not in _nats_ctr.securityContext

# ── (1b) temporal-like: run_as_non_root=False (root) + writable rootfs ─────
_temporal = forge.Service {
    name = "temporal"
    image = "temporalio/auto-setup:1.26.2"
    ports = [7233]
    security = forge.SecurityOverrides {
        run_as_non_root = False
        run_as_user = 0
        read_only_root_fs = False
    }
}
_t_manifests = forge.render_manifests(forge.Bundle {cluster_target = _target, workloads = [_temporal]}, "latest", {}, False)
_t_ctr = [m for m in _t_manifests if m.kind == "Deployment" and m.metadata.name == "temporal"][0].spec.template.spec.containers[0]
assert_temporal_root = _t_ctr.securityContext.runAsNonRoot == False and _t_ctr.securityContext.runAsUser == 0
assert_temporal_writable = _t_ctr.securityContext.readOnlyRootFilesystem == False

# ── (2) sidecar + init containers ──────────────────────────────────────────
_app = forge.Service {
    name = "api"
    image = "control-plane"
    command = ["/app/control-plane"]
    args = ["api"]
    ports = [8080]
    env = {
        "PORT" = {value = "8080"}
    }
    # A schema-migration init container that runs to completion first.
    init = [
        forge.Container {
            name = "migrate"
            image = "control-plane"
            command = ["/app/control-plane"]
            args = ["migrate"]
            env = {"DATABASE_URL" = {from_secret = {name = "db-credentials", key = "url"}}}
        }
    ]
    # An OTel-collector sidecar alongside the primary, with a Downward-API env.
    sidecars = [
        forge.Container {
            name = "otel-collector"
            image = "otel/opentelemetry-collector:0.100.0"
            ports = [4317]
            env = {
                "POD_NAME" = {from_field = "metadata.name"}
                "POD_NAMESPACE" = {from_field = "metadata.namespace"}
            }
        }
    ]
}

_app_manifests = forge.render_manifests(forge.Bundle {cluster_target = _target, workloads = [_app]}, "latest", {}, False)
_app_pod = [m for m in _app_manifests if m.kind == "Deployment" and m.metadata.name == "api"][0].spec.template.spec

# Primary is FIRST, sidecar appended AFTER it.
assert_ctr_count = len(_app_pod.containers) == 2
assert_primary_first = _app_pod.containers[0].name == "api"
assert_sidecar_after = _app_pod.containers[1].name == "otel-collector"
# Sidecar image resolved via the SAME registry/tag machinery as the primary
# (its own tag pins it, so the registry prefix is added but tag preserved).
assert_sidecar_image = _app_pod.containers[1].image == "ghcr.io/reliant-labs/otel/opentelemetry-collector:0.100.0"
assert_sidecar_port = _app_pod.containers[1].ports[0].containerPort == 4317
# Sidecar env projected — Downward API fieldRef included.
_side_env = {e.name: e for e in _app_pod.containers[1].env}
assert_sidecar_downward = _side_env["POD_NAME"].valueFrom.fieldRef == {fieldPath = "metadata.name"}
# Sidecar keeps the restricted-PSA container SC.
assert_sidecar_sc = _app_pod.containers[1].securityContext.readOnlyRootFilesystem == True

# Init container present as initContainers, image + command resolved.
assert_init_present = len(_app_pod.initContainers) == 1
assert_init_name = _app_pod.initContainers[0].name == "migrate"
assert_init_command = _app_pod.initContainers[0].command == ["/app/control-plane", "migrate"]
assert_init_env = _app_pod.initContainers[0].env[0].valueFrom.secretKeyRef == {name = "db-credentials", key = "url"}

# ── (2b) health probe -> liveness + readiness (k8s) + healthcheck (compose) ─
_probed = forge.Service {
    name = "probed"
    image = "control-plane"
    ports = [8080, 9090]
    health = forge.HealthProbe {
        http_path = "/healthz"
        period_seconds = 15
        failure_threshold = 5
    }
}
_probed_ctr = [m for m in forge.render_manifests(forge.Bundle {cluster_target = _target, workloads = [_probed]}, "latest", {}, False) if m.kind == "Deployment" and m.metadata.name == "probed"][0].spec.template.spec.containers[0]
# ONE config rendered as BOTH probes; port defaults to the FIRST declared port.
assert_liveness = _probed_ctr.livenessProbe.httpGet == {path = "/healthz", port = 8080}
assert_readiness = _probed_ctr.readinessProbe.httpGet == {path = "/healthz", port = 8080}
assert_probe_same = _probed_ctr.livenessProbe == _probed_ctr.readinessProbe
assert_probe_timing = _probed_ctr.livenessProbe.periodSeconds == 15 and _probed_ctr.livenessProbe.failureThreshold == 5
# tcp probe on an explicit port -> tcpSocket.
_tcp_probed = forge.Service {
    name = "tcp-probed"
    image = "nats"
    ports = [4222]
    health = forge.HealthProbe {tcp = True, port = 8222}
}
_tcp_ctr = [m for m in forge.render_manifests(forge.Bundle {cluster_target = _target, workloads = [_tcp_probed]}, "latest", {}, False) if m.kind == "Deployment"][0].spec.template.spec.containers[0]
assert_tcp_probe = _tcp_ctr.livenessProbe.tcpSocket == {port = 8222} and "httpGet" not in _tcp_ctr.livenessProbe
# compose adapter -> healthcheck (HTTP GET via wget, timing mapped).
_probed_compose = forge._service_to_compose(_probed, "reg", "tag", "/secrets")
assert_compose_hc_test = _probed_compose.healthcheck.test == ["CMD", "wget", "--spider", "-q", "http://localhost:8080/healthz"]
assert_compose_hc_interval = _probed_compose.healthcheck.interval == "15s"
assert_compose_hc_retries = _probed_compose.healthcheck.retries == 5

# ── (3) HAPPY PATH UNCHANGED — no security/sidecars/init/health == identical ─
# The SAME plain service authored as an agnostic Service vs a hand-authored
# RenderedWorkload baseline must render an IDENTICAL Deployment.
_plain = forge.Service {
    name = "plain"
    image = "control-plane"
    command = ["/app/control-plane"]
    ports = [8080]
    env = {"PORT" = {value = "8080"}}
}
_baseline = forge.RenderedWorkload {
    name = "plain"
    image = "control-plane"
    command = ["/app/control-plane"]
    env_vars = [forge.EnvVar {name = "PORT", value = "8080"}]
    deploy = _target.deploy | {ports = [8080], replicas = 1}
}
_plain_dep = [m for m in forge.render_manifests(forge.Bundle {cluster_target = _target, workloads = [_plain]}, "latest", {}, False) if m.kind == "Deployment"][0]
_baseline_dep = [m for m in forge.render_manifests(forge.Bundle {cluster_target = _target, services = [_baseline]}, "latest", {}, False) if m.kind == "Deployment"][0]

# The single-container happy path is untouched: NO initContainers key, exactly
# one container, and the FULL pod spec is byte-identical to the baseline.
assert_plain_no_init = "initContainers" not in _plain_dep.spec.template.spec
assert_plain_one_ctr = len(_plain_dep.spec.template.spec.containers) == 1
# No `health` => NO probes on the container (the dead-probe gap stays closed
# without changing the probe-free default).
assert_plain_no_liveness = "livenessProbe" not in _plain_dep.spec.template.spec.containers[0]
assert_plain_no_readiness = "readinessProbe" not in _plain_dep.spec.template.spec.containers[0]
assert_plain_identical = _plain_dep.spec.template.spec == _baseline_dep.spec.template.spec
assert_plain_default_pod_sc = _plain_dep.spec.template.spec.securityContext == {
    runAsNonRoot = True
    runAsUser = 65532
    runAsGroup = 65532
    seccompProfile = {type = "RuntimeDefault"}
}
