"""
Positive tests for the K8sCluster deploy target.

K8sCluster is the per-service deploy block that ALSO carries the
env-wide knobs (cluster/namespace/registry/domain) so KCL refs can DRY
the common case across many services. Exercises:

  * Reference-and-share via assignment to a `_prod_k8s` var.
  * Per-service override via `_prod_k8s | { replicas = 5 }`.
  * Discriminator projects as "cluster".
  * env-wide fields project on the rendered output (not null).
"""

import forge

# Declare once at the top — share via ref across many services.
_prod_k8s = forge.K8sCluster {
    cluster = "prod-cluster"
    namespace = "kalshi-prod"
    registry = "ghcr.io/reliant/kalshi"
    domain = "kalshi.com"
}

_svc_a = forge.Service {
    name = "trader"
    deploy = _prod_k8s
}

# Per-service override via KCL's `|` merge — replicas bumped to 5,
# everything else stays from _prod_k8s.
_svc_b = forge.Service {
    name = "admin"
    deploy = _prod_k8s | {replicas = 5}
}

_bundle = forge.Bundle {
    services = [_svc_a, _svc_b]
}

output = forge.render(_bundle)

# ── Invariants ─────────────────────────────────────────────────────

# Discriminator is "cluster" for K8sCluster.
assert_a_discriminator = output.services[0].deploy.type == "cluster"
assert_b_discriminator = output.services[1].deploy.type == "cluster"

# Env-wide fields project on the rendered output (not null).
assert_a_cluster = output.services[0].deploy.cluster == "prod-cluster"
assert_a_namespace = output.services[0].deploy.namespace == "kalshi-prod"
assert_a_registry = output.services[0].deploy.registry == "ghcr.io/reliant/kalshi"
assert_a_domain = output.services[0].deploy.domain == "kalshi.com"

# `|` merge preserves env-wide fields while overriding replicas.
assert_b_cluster = output.services[1].deploy.cluster == "prod-cluster"
assert_b_replicas = output.services[1].deploy.replicas == 5
assert_a_replicas = output.services[0].deploy.replicas == 1

# Default values for new K8sCluster optional fields.
assert_a_env_vars_empty = len(output.services[0].deploy.env_vars) == 0
assert_a_ingress_null = output.services[0].deploy.ingress == None
assert_a_health_check_null = output.services[0].deploy.health_check == None
