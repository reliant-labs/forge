"""
Positive test — the SimpleBackend deploy tier.

SimpleBackend is a CONSTRAINED PROFILE over K8sCluster, not a parallel
mechanism: `render_manifests` projects it onto the same RenderedWorkload
the k8s adapter emits (`_project_simple_backend`), so the manifests come
out of the SAME builders every forge.Service uses. This fixture pins the
claims that make that true, plus the two places the tier deliberately
differs.

  1. ENTITY contract keeps the `simple-backend` discriminator. Manifests
     take the cluster path, but a tier that flattened to "cluster" in the
     entity output would be invisible to `forge project audit`.
  2. The app owner's PINNED, fully-qualified image survives verbatim — the
     projection sets registry = "" precisely so neither the env registry
     nor the env-wide image_tag can rewrite a reference forge did not
     build. `image_tag` here is a real value, not "latest", so a
     regression that let the tag through would change the string.
  3. Deployment + Service + RBAC come from the shared builders: replicas
     pinned to 1, resources mapped through the neutral->k8s formatter,
     containerPort and Service port correct.
  4. env_vars reach the container across all three channels (value /
     secretKeyRef / configMapKeyRef) with NO new env machinery — they are
     ordinary forge.EnvVars.
  5. health_check lands as BOTH liveness and readiness, via
     RenderedWorkload.health_probe. This is the trap _project_frontend
     documents: the deploy block's `health_check` is the host/compose
     -shaped field and the k8s renderer does not read it, so getting this
     backwards yields a workload with silently no probes.
  6. storage_gib EMITS a PersistentVolumeClaim (the one genuinely new
     manifest kind in this work — forge elsewhere only references a PVC
     by name) and mounts it at /data.

The `network` modes have their own fixture (positive_simple_backend_network.k).
Off-band resource shapes have theirs (negative_simple_backend_*.k).
"""

import forge

_api = forge.RenderedWorkload {
    name = "acme-api"
    deploy = forge.SimpleBackend {
        cluster = "gke_acme_us-central1_hosted"
        namespace = "acme-prod"
        image = "ghcr.io/acme/api:v1.4.2"
        ports = [8080]
        network = "public"
        domain = "api-acme.reliantapps.dev"
        storage_gib = 20
        resources = forge.Resources {
            cpu_request_millicores = 500
            cpu_limit_millicores = 1000
            memory_request_bytes = forge.gib(2)
            memory_limit_bytes = forge.gib(2)
        }
        health_check = forge.HealthCheck {
            liveness_path = "/healthz"
            http_port = 8080
            initial_delay = 7
        }
        env_vars = [
            forge.EnvVar {name = "LOG_LEVEL", value = "info"}
            forge.EnvVar {name = "DB_PASSWORD", secret_ref = "acme-prod-db", secret_key = "password"}
            forge.EnvVar {name = "REGION", config_map_ref = "acme-prod-config", config_map_key = "region"}
        ]
    }
}

# cluster_target is the ENV-wide k8s coordinate block. A SimpleBackend does
# NOT read it (it carries its own cluster/namespace, and its registry is
# deliberately empty), but Bundle requires one whenever anything k8s-shaped
# is present. Its registry is set to a DIFFERENT registry than the image's
# on purpose: assertion (2) below is what proves it was not applied.
_target = forge.ClusterTarget {
    cluster = "gke_acme_us-central1_hosted"
    namespace = "acme-prod"
    registry = "ghcr.io/reliant-labs"
}

_bundle = forge.Bundle {
    project = "hosted"
    cluster_target = _target
    services = [_api]
}

_out = forge.render(_bundle)
_manifests = forge.render_manifests(_bundle, "v9-should-not-appear", {}, False)

_svc_entity = _out.services[0]
_deploy = _svc_entity.deploy

_deployment = [m for m in _manifests if m.kind == "Deployment"][0]
_service = [m for m in _manifests if m.kind == "Service"][0]
_pvc = [m for m in _manifests if m.kind == "PersistentVolumeClaim"][0]
_container = _deployment.spec.template.spec.containers[0]
_env = {e.name: e for e in _container.env}

# ── (1) the entity contract keeps the discriminator ────────────────────
assert_entity_type = _deploy.type == "simple-backend"
assert_entity_cluster = _deploy.cluster == "gke_acme_us-central1_hosted"
assert_entity_namespace = _deploy.namespace == "acme-prod"
assert_entity_network = _deploy.network == "public"
assert_entity_domain = _deploy.domain == "api-acme.reliantapps.dev"
assert_entity_storage = _deploy.storage_gib == 20
assert_entity_image = _deploy.image == "ghcr.io/acme/api:v1.4.2"
# resources project UNCONDITIONALLY (the schema defaults them) — a billing
# input the consumer must never have to re-derive.
assert_entity_resources_cpu = _deploy.resources.cpu_request_millicores == 500
assert_entity_resources_mem = _deploy.resources.memory_request_bytes == 2147483648

# ── (2) the app owner's pinned image survives verbatim ────────────────────
# Neither the env registry (ghcr.io/reliant-labs) nor the env-wide tag
# ("v9-should-not-appear") may touch a reference forge did not build.
assert_image_verbatim = _container.image == "ghcr.io/acme/api:v1.4.2"

# ── (3) the shared cluster builders produced the workload ──────────────
assert_deployment_name = _deployment.metadata.name == "acme-api"
assert_deployment_namespace = _deployment.metadata.namespace == "acme-prod"
assert_replicas_pinned_to_one = _deployment.spec.replicas == 1
assert_container_port = _container.ports[0].containerPort == 8080
assert_service_port = _service.spec.ports[0].port == 8080
assert_cpu_request = _container.resources.requests.cpu == "500m"
assert_mem_request = _container.resources.requests.memory == "2Gi"
assert_cpu_limit = _container.resources.limits.cpu == "1"
# RBAC comes from the shared path too — a ServiceAccount is what carries it.
assert_service_account = len([m for m in _manifests if m.kind == "ServiceAccount"]) == 1

# ── (4) all three env channels, no new machinery ───────────────────────
assert_env_value = _env["LOG_LEVEL"].value == "info"
assert_env_secret = _env["DB_PASSWORD"].valueFrom.secretKeyRef.name == "acme-prod-db"
assert_env_secret_key = _env["DB_PASSWORD"].valueFrom.secretKeyRef.key == "password"
assert_env_config = _env["REGION"].valueFrom.configMapKeyRef.name == "acme-prod-config"

# ── (5) health_check reached the k8s probe path ────────────────────────
# The trap: had this projected onto the deploy block's health_check
# instead of RenderedWorkload.health_probe, both probes would be ABSENT
# and nothing else in this fixture would notice.
assert_liveness = _container.livenessProbe.httpGet.path == "/healthz"
assert_readiness = _container.readinessProbe.httpGet.path == "/healthz"
assert_probe_delay = _container.livenessProbe.initialDelaySeconds == 7

# ── (6) storage_gib EMITS a PVC and mounts it ──────────────────────────
assert_pvc_name = _pvc.metadata.name == "acme-api-data"
assert_pvc_namespace = _pvc.metadata.namespace == "acme-prod"
assert_pvc_size = _pvc.spec.resources.requests.storage == "20Gi"
assert_pvc_access_mode = _pvc.spec.accessModes == ["ReadWriteOnce"]
assert_volume_mounted = _container.volumeMounts[0].mountPath == "/data"
assert_volume_claim = _deployment.spec.template.spec.volumes[0].persistentVolumeClaim.claimName == "acme-api-data"
