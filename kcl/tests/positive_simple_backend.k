"""
Positive test — the SimpleBackend deploy tier.

A SimpleBackend's spec is the generated tiers.SimpleBackend (from forge's
Go type pkg/deploy/v1alpha1.SimpleBackendSpec). render_manifests emits it as
ONE forge.dev/v1alpha1 declaration record, the same object the hosted path
publishes as a CR. forge's manifest extraction expands that record through
the Go renderer (pkg/deploy.Render). So what KCL is responsible for, and
what this fixture pins, is:

  1. The ENTITY contract keeps the `simple-backend` discriminator and carries
     the target (cluster, namespace) plus the spec in its CR JSON shape.
  2. The manifest stream carries exactly one declaration record per backend:
     namespaced to the backend's own namespace, labelled with the managed
     set and the env stamp, and spec = the CR JSON with unset optionals
     ABSENT (never null, which would clear a CR field).
  3. The working defaults applied: omitted resources are the 250m / 1 GiB
     entry shape, and network is private.
  4. No KCL builder expanded the backend. No Deployment appears in the
     KCL-level stream. The Deployment/Service/PVC objects are Go's, and
     internal/cli TestSimpleBackend_* assert them against the real render.
"""

import forge
import forge.tiers

_api = forge.RenderedWorkload {
    name = "acme-api"
    deploy = forge.SimpleBackend {
        cluster = "gke_acme_us-central1_hosted"
        namespace = "acme-prod"
        spec = tiers.SimpleBackend {
            image = "ghcr.io/acme/api:v1.4.2"
            ports = [8080]
            network = "public"
            domains = ["api.acme.com"]
            storageGiB = 20
            healthCheck = tiers.HealthCheck { port = 8080, path = "/healthz", initialDelaySeconds = 7 }
            env = [
                tiers.EnvVar { name = "LOG_LEVEL", value = "info" }
                tiers.EnvVar { name = "DB_PASSWORD", secretRef = tiers.SecretKeyRef { name = "acme-prod-db", key = "password" } }
                tiers.EnvVar { name = "DATABASE_URL", databaseRef = tiers.DatabaseRef { name = "orders" } }
            ]
        }
    }
}

_bundle = forge.Bundle {
    project = "hosted"
    env = "prod"
    cluster_target = forge.ClusterTarget {
        cluster = "gke_acme_us-central1_hosted"
        namespace = "acme-prod"
        registry = "ghcr.io/reliant-labs"
    }
    services = [_api]
}

_out = forge.render(_bundle)
_manifests = forge.render_manifests(_bundle, "v9-should-not-appear", {}, False)

_deploy = _out.services[0].deploy
_records = [m for m in _manifests if m.kind == "SimpleBackend"]
_record = _records[0]

# ── (1) entity contract ─────────────────────────────────────────────────
assert_entity_type = _deploy.type == "simple-backend"
assert_entity_cluster = _deploy.cluster == "gke_acme_us-central1_hosted"
assert_entity_namespace = _deploy.namespace == "acme-prod"
assert_entity_spec_image = _deploy.spec.image == "ghcr.io/acme/api:v1.4.2"
assert_entity_spec_storage = _deploy.spec.storageGiB == 20

# ── (2) exactly one declaration record, shaped as the hosted CR ──────────
assert_one_record = len(_records) == 1
assert_record_group = _record.apiVersion == "forge.dev/v1alpha1"
assert_record_name = _record.metadata.name == "acme-api"
assert_record_namespace = _record.metadata.namespace == "acme-prod"
assert_record_part_of = _record.metadata.labels["app.kubernetes.io/part-of"] == "hosted"
assert_record_env_stamp = _record.metadata.labels["forge.dev/env"] == "prod"
assert_record_image_verbatim = _record.spec.image == "ghcr.io/acme/api:v1.4.2"
assert_record_domains = _record.spec.domains == ["api.acme.com"]
assert_record_probe = _record.spec.healthCheck.path == "/healthz"
assert_record_db_ref = _record.spec.env[2].databaseRef.name == "orders"
# Unset optionals are ABSENT, not null: the databaseRef entry has no key.
assert_record_no_null_key = "key" not in _record.spec.env[2].databaseRef
assert_record_no_value_on_secret = "value" not in _record.spec.env[1]

# ── (3) working defaults ────────────────────────────────────────────────
_min = forge.render_manifests(forge.Bundle {
    project = "p"
    cluster_target = forge.ClusterTarget { cluster = "c", namespace = "n", registry = "r.io/x" }
    services = [forge.RenderedWorkload {
        name = "w"
        deploy = forge.SimpleBackend { cluster = "c", namespace = "n", spec = tiers.SimpleBackend { image = "ghcr.io/a/w:v1", ports = [3000] } }
    }]
}, "t", {}, False)
_min_spec = [m for m in _min if m.kind == "SimpleBackend"][0].spec
assert_default_network = _min_spec.network == "private"
assert_default_cpu = _min_spec.resources.cpuRequestMillicores == 250
assert_default_mem = _min_spec.resources.memoryRequestBytes == 1073741824

# ── (4) no KCL-side expansion ───────────────────────────────────────────
assert_no_kcl_deployment = len([m for m in _manifests if m.kind == "Deployment"]) == 0
assert_no_kcl_pvc = len([m for m in _manifests if m.kind == "PersistentVolumeClaim"]) == 0
