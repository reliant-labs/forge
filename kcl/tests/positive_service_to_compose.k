"""
PROOF — the SAME agnostic `forge.Service` renders to BOTH targets.

This is the acid test of the target-agnostic core (§7 FORGE_KCL_IDIOMS.md).
We take ONE agnostic Service — modeled on the prod workspace-proxy dogfood
(positive_workspace_proxy_dogfood.k) — and render it TWO ways from the ONE
authored spec:

  1. k8s  — via the shipping path: Bundle.workloads -> render_manifests
            (the k8s adapter `_service_to_k8s` runs INSIDE).
  2. compose — via the SKETCH adapter `_service_to_compose`.

We then assert the compose service is correct across every channel, and —
critically — that the k8s escape-hatch block (`svc.k8s`) did NOT bleed into
the compose output. If the same core Service renders cleanly to both, the
core is honestly agnostic; anything that resists reveals a baked-in k8s
assumption. The verdict is at the bottom.
"""

import forge

# ── ONE agnostic Service (no k8s vocabulary except the namespaced escape) ──
# Same shape as the prod workspace-proxy: all three env channels, env_from,
# resources, a mounted secret volume, and a k8s escape hatch (annotations +
# raw_manifests) that a compose adapter must ignore.
_healthcheck_policy = {
    apiVersion = "networking.gke.io/v1"
    kind = "HealthCheckPolicy"
    metadata = {name = "workspace-proxy"}
    spec = {targetRef = {group = "", kind = "Service", name = "workspace-proxy"}}
}

svc = forge.Service {
    name = "workspace-proxy"
    image = "control-plane"
    command = ["/app/control-plane"]
    args = ["workspace-proxy"]
    ports = [8080]
    replicas = 2
    resources = forge.Resources {
        cpu_request_millicores = forge.cpu(0.25)   # 250m k8s / 0.25 compose
        cpu_limit_millicores = forge.cpu(1)        # 1
        memory_request_bytes = forge.mib(256)      # 256Mi k8s / bytes compose
        memory_limit_bytes = forge.mib(512)        # 512Mi
    }
    env = {
        # value channel
        "OTEL_SERVICE_NAME" = {value = "workspace-proxy"}
        "PORT" = {value = "8080"}
        # from_secret channel
        "INTERNAL_SERVICE_SECRET" = {from_secret = {name = "control-plane-internal", key = "secret"}}
        # from_config channel
        "LOG_LEVEL" = {from_config = {name = "control-plane-config", key = "log-level"}}
    }
    # whole-configmap bulk import
    env_from = [forge.EnvFrom {config = "control-plane-proxy-extra"}]
    # the prod cross-cluster mount: a Secret, read-only
    volumes = [
        forge.Volume {
            name = "daemon-cluster-kubeconfig"
            mount_path = "/etc/daemon-cluster"
            type = "secret"
            source = "cp-daemon-kubeconfig"
            read_only = True
        }
    ]
    # the k8s escape hatch — MUST be ignored by the compose adapter.
    k8s = forge.K8sOverrides {
        annotations = {"reliant.dev/scrape" = "true"}
        raw_manifests = [_healthcheck_policy]
    }
}

# ── Target 1: k8s (the shipping path) ──────────────────────────────────────
_prod = forge.ClusterTarget {
    cluster = "control-plane-prod-cluster"
    namespace = "control-plane-prod"
    registry = "ghcr.io/reliant-labs"
    platform = "amd64"
}
_manifests = forge.render_manifests(forge.Bundle {cluster_target = _prod, workloads = [svc]}, "v1.4.2", {}, False)
_dep = [m for m in _manifests if m.kind == "Deployment" and m.metadata.name == "workspace-proxy"][0]
_ctr = _dep.spec.template.spec.containers[0]

# ── Target 2: compose (the SKETCH adapter) — SAME `svc` ─────────────────────
compose = forge._service_to_compose(svc, "ghcr.io/reliant-labs", "v1.4.2", "./secrets")

# ═══════════════════════════════════════════════════════════════════════════
# PROOF 1 — image projects IDENTICALLY to both targets (trivially agnostic).
# ═══════════════════════════════════════════════════════════════════════════
assert_k8s_image = _ctr.image == "ghcr.io/reliant-labs/control-plane:v1.4.2"
assert_compose_image = compose.image == "ghcr.io/reliant-labs/control-plane:v1.4.2"
assert_image_identical = compose.image == _ctr.image

# ── command+args fold the same way for both ────────────────────────────────
assert_compose_command = compose.command == ["/app/control-plane", "workspace-proxy"]

# ═══════════════════════════════════════════════════════════════════════════
# PROOF 2 — env: the SAME map, projected two ways. k8s -> LIST + refs;
# compose -> MAP, three channels lowered three different ways.
# ═══════════════════════════════════════════════════════════════════════════
# compose `environment` is a MAP (the point: no dedup, near-identity for values).
# value channel -> inline literal.
assert_compose_env_value = compose.environment["OTEL_SERVICE_NAME"] == "workspace-proxy"
assert_compose_env_port = compose.environment["PORT"] == "8080"
# from_config channel -> ${VAR} interpolation (no ConfigMap in compose).
assert_compose_env_config = compose.environment["LOG_LEVEL"] == "$" + "{LOG_LEVEL}"
# from_secret channel -> NOT in `environment`; lowered to an env_file (below).
assert_compose_env_no_secret = "INTERNAL_SERVICE_SECRET" not in compose.environment
# environment holds exactly the 2 value + 1 config channels (3), not the secret.
assert_compose_env_count = len(compose.environment) == 3

# Cross-check: the SAME channels on k8s become container `env` LIST entries.
_k8s_env = {e.name: e for e in _ctr.env}
assert_k8s_env_value = _k8s_env["OTEL_SERVICE_NAME"].value == "workspace-proxy"
assert_k8s_env_secret = _k8s_env["INTERNAL_SERVICE_SECRET"].valueFrom.secretKeyRef == {name = "control-plane-internal", key = "secret"}
assert_k8s_env_config = _k8s_env["LOG_LEVEL"].valueFrom.configMapKeyRef == {name = "control-plane-config", key = "log-level"}

# ── from_secret + env_from -> compose env_file (the secret/config lowering) ─
# One env_file for the from_secret's owning Secret, one for the env_from config.
assert_compose_env_file = compose.env_file == ["./secrets/control-plane-internal.env", "./secrets/control-plane-proxy-extra.env"]

# ═══════════════════════════════════════════════════════════════════════════
# PROOF 3 — ports / replicas / resources / volumes project cleanly.
# ═══════════════════════════════════════════════════════════════════════════
assert_compose_ports = compose.ports == ["8080:8080"]
assert_compose_replicas = compose.deploy.replicas == 2
# resources -> deploy.resources limits/reservations. The compose adapter
# FORMATS the neutral quantities for compose: millicores -> decimal cores
# ("250" -> "0.25", "1000" -> "1"), bytes -> raw byte int. NO k8s dialect
# ("250m"/"256Mi") leaks through — the leak the earlier proof flagged is fixed.
assert_compose_res_limits = compose.deploy.resources.limits == {cpus = "1", memory = 536870912}
assert_compose_res_reservations = compose.deploy.resources.reservations == {cpus = "0.25", memory = 268435456}
# volumes -> "source:target:ro" (the mounted Secret, read-only).
assert_compose_volumes = compose.volumes == ["cp-daemon-kubeconfig:/etc/daemon-cluster:ro"]

# ═══════════════════════════════════════════════════════════════════════════
# PROOF 4 — the k8s ESCAPE HATCH did NOT bleed into compose.
# The compose adapter never reads svc.k8s, so none of its content appears.
# ═══════════════════════════════════════════════════════════════════════════
# The Tier-1 annotation lives on the k8s pod template...
assert_k8s_has_annotation = _dep.spec.template.metadata.annotations == {"reliant.dev/scrape" = "true"}
# ...but NOWHERE in the compose output.
_compose_keys = [_k for _k in compose]
assert_compose_no_annotations = "annotations" not in _compose_keys
assert_compose_no_raw_manifests = "raw_manifests" not in _compose_keys
assert_compose_no_k8s = "k8s" not in _compose_keys
# The Tier-2 raw HealthCheckPolicy IS emitted by the k8s adapter...
assert_k8s_hcp = len([m for m in _manifests if m.kind == "HealthCheckPolicy"]) == 1
# ...and its distinctive marker does NOT appear anywhere in compose (deep check
# via string render — proves the escape hatch is fully namespaced away).
assert_compose_no_hcp = "HealthCheckPolicy" not in str(compose)
assert_compose_no_scrape = "reliant.dev/scrape" not in str(compose)

# The compose service has exactly the portable keys — no k8s tail.
assert_compose_keys = sorted(_compose_keys) == sorted(["image", "command", "environment", "env_file", "ports", "volumes", "deploy"])

# ═══════════════════════════════════════════════════════════════════════════
# VERDICT (honest):
#
# The core is AGNOSTIC. The SAME `svc` rendered to both targets with zero
# edits. What projected TRIVIALLY (genuinely target-neutral):
#   * image        — identical ref on both (registry/tag resolution is agnostic)
#   * command/args — same argv fold
#   * ports        — int list -> "8080:8080" vs containerPort, both mechanical
#   * replicas     — deploy.replicas vs Deployment.replicas
#   * volumes      — source+path+ro -> one string vs pod volume + mount
#   * env (the KEY win) — the MAP is the RIGHT core. k8s needs a list+dedup;
#     compose keeps it a MAP. The abstract from_secret/from_config refs lowered
#     THREE ways for compose (inline / ${VAR} / env_file) vs the k8s LIST
#     (value / secretKeyRef / configMapKeyRef). Core names WHAT; adapter HOW.
#   * k8s escape hatch — target-NAMESPACED, so it vanished cleanly. Proof the
#     non-portable tail cannot leak into a non-k8s target.
#
# Resources — the ONE field that once RESISTED, now FIXED. forge.Resources
#     formerly held k8s-shaped quantity STRINGS ("250m" cpu, "256Mi" memory),
#     which the compose adapter passed through verbatim — a k8s dialect leak.
#     It now holds NEUTRAL quantities (cpu millicores int, memory bytes int)
#     and each adapter FORMATS for its target (lib/quantity.k): the k8s
#     adapter reproduces "250m"/"256Mi", the compose adapter emits "0.25" cores
#     and raw bytes. Same neutral core, two encodings — exactly as env is a
#     neutral map each adapter projects. The core is now honestly agnostic in
#     EVERY field. Authors write forge.cpu(0.25) / forge.mib(256).
# ═══════════════════════════════════════════════════════════════════════════
