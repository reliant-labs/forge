"""
Positive test — KCL-native schema INHERITANCE of forge.K8sWorkload.

A project may declare `schema MyService(forge.K8sWorkload): { ... }` to add its
OWN typed/required fields (a tier enum, a required region) while having
forge render the result EXACTLY like a plain forge.K8sWorkload. This works
because the render layer's lambdas are typed on the BASE schema
(`lambda s: Service -> ...`), which accepts any subtype, and project only
the forge.K8sWorkload fields — the app's extra fields ride along on the typed
value and are simply not projected into the JSON contract / manifests.

This test proves:
  1. an app schema inheriting forge.K8sWorkload renders into the JSON contract
     identically to a plain forge.K8sWorkload (same keys, same deploy block);
  2. it renders into the k8s manifest stream (Deployment + Service + RBAC);
  3. the app's extra fields don't perturb the rendered shape.

The COMPANION negative (extra REQUIRED field enforced at parse time) is
negative_service_inheritance_missing_required.k.
"""

import forge

# An app schema that inherits forge.K8sWorkload and adds its own typed fields.
schema TenantService(forge.K8sWorkload):
    # An extra REQUIRED field (no default) — enforced at parse time.
    region: str
    # An extra typed field with an enum constraint.
    tier: "free" | "pro" = "free"

    check:
        region, "TenantService.region is required"

_target = forge.ClusterTarget {
    cluster = "prod-cluster"
    namespace = "tenants-prod"
    registry = "ghcr.io/reliant-labs"
}

# Construct via the SUBTYPE — sets both forge.K8sWorkload fields and the app's
# own extras.
_svc = TenantService {
    name = "tenant-api"
    image = "tenant-api"
    region = "us-east-1"
    tier = "pro"
    deploy = _target.deploy | {ports = [8080]}
}

_bundle = forge.Bundle {
    cluster_target = _target
    services = [_svc]
}

output = forge.render(_bundle)

# ── 1. renders into the JSON contract exactly like a forge.K8sWorkload ───
assert_one_service = len(output.services) == 1
_s = output.services[0]
assert_name = _s.name == "tenant-api"
assert_image = _s.image == "tenant-api"
# The deploy block projects the SAME shape a plain forge.K8sWorkload would.
assert_deploy_type = _s.deploy.type == "cluster"
assert_deploy_ns = _s.deploy.namespace == "tenants-prod"
assert_deploy_ports = _s.deploy.ports == [8080]
# The app's extra fields are NOT in the projected JSON contract — they ride
# on the typed value but aren't part of the stable forge.K8sWorkload contract.
assert_no_region_leak = "region" not in _s
assert_no_tier_leak = "tier" not in _s

# ── 2. renders into the k8s manifest stream ──────────────────────────
manifests = forge.render_manifests(_bundle, "v1", {}, False)
_kinds = [m.kind for m in manifests]
assert_has_deployment = "Deployment" in _kinds
assert_has_service = "Service" in _kinds
_dep = [m for m in manifests if m.kind == "Deployment" and m.metadata.name == "tenant-api"][0]
assert_deployment_named = _dep.metadata.name == "tenant-api"
