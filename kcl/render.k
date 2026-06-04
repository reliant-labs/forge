"""
Forge KCL — render layer.

Two responsibilities:

  1. `render(bundle)` — project a typed `Bundle` to the stable JSON
     contract documented in `schema.k`. This is what the forge CLI
     (`forge build`, `forge run`, `forge deploy`, etc.) consumes via
     `kcl run deploy/kcl/<env>/ -S output --format json`.

  2. `render_manifests(bundle, env)` — emit the Kubernetes manifest
     list for the cluster-targeted entities (K8sCluster services,
     operators, scheduled cronjobs). Host-mode services and BuildOnly
     services are skipped — those live outside the cluster manifest
     boundary.

Missing optional fields project as JSON `null` (not omitted) so the
consumer sees stable keys.

This file is a SIBLING of `schema.k` so it can reference Bundle,
Service, etc. as bare names — no `import .schema` (which would
fork the type identity, blocking project code that does `import
forge`).

KCL union-type dispatch
=======================

`Service.deploy: HostDeploy | K8sCluster | VMDocker | Compose | BuildOnly`.
The discriminator field (`type`) is declared ON each variant subschema,
so the JSON projection is self-describing — the consumer dispatches
on `deploy.type` directly. The render lambdas here mirror that
contract: one branch per discriminator value, always producing a
fully-populated dict so the JSON shape is stable.
"""

import .lib.services as svc_lib
import .lib.rbac as rbac_lib
import .lib.netpol as netpol_lib

# ---------------------------------------------------------------------------
# JSON contract render — typed Bundle -> stable dict shape.
# ---------------------------------------------------------------------------

# Render an EnvVar to its stable dict form. Empty-string defaults
# project as `null` so the consumer sees stable keys.
_render_env_var = lambda ev: EnvVar -> any {
    {
        "name": ev.name
        "value": ev.value if ev.value else None
        "secret_ref": ev.secret_ref if ev.secret_ref else None
        "secret_key": ev.secret_key if ev.secret_key else None
        "config_map_ref": ev.config_map_ref if ev.config_map_ref else None
        "config_map_key": ev.config_map_key if ev.config_map_key else None
    }
}

_render_ingress = lambda ing: Ingress -> any {
    {
        "host": ing.host
        "tls": ing.tls
        "cert_manager_issuer": ing.cert_manager_issuer
        "path": ing.path
    }
}

_render_resources = lambda r: Resources -> any {
    {
        "cpu_request": r.cpu_request
        "cpu_limit": r.cpu_limit
        "memory_request": r.memory_request
        "memory_limit": r.memory_limit
    }
}

# Lifted above _render_k8s_deploy so K8sCluster's optional cluster_rbac
# field can project through the deploy dispatch. Operators consume the
# same helper from below.
_render_cluster_rbac = lambda rbac: ClusterRBAC -> any {
    {"rules": rbac.rules}
}

_render_build_variant = lambda bv: BuildVariant -> any {
    {
        "name": bv.name
        "ldflags": bv.ldflags
        "tags": bv.tags
        "goos": bv.goos if bv.goos else None
        "goarch": bv.goarch if bv.goarch else None
    }
}

# Dispatch on the `type` discriminator. Each branch returns a
# fully-populated dict so the consumer sees identical key shape for
# every member of the union.
_render_host_deploy = lambda d: HostDeploy -> any {
    {
        "type": "host"
        "runner": d.runner
        "air_config": d.air_config if d.air_config else None
        "env_vars": [_render_env_var(e) for e in d.env_vars]
        "secrets_file": d.secrets_file if d.secrets_file else None
        "delve_port": d.delve_port
    }
}

# Renders K8sCluster — env-wide knobs (cluster/namespace/registry/domain)
# plus the per-service knobs.
_render_k8s_cluster = lambda d: K8sCluster -> any {
    {
        "type": "cluster"
        "cluster": d.cluster
        "namespace": d.namespace
        "registry": d.registry
        "domain": d.domain if d.domain else None
        "replicas": d.replicas
        "ingress": _render_ingress(d.ingress) if d.ingress else None
        "resources": _render_resources(d.resources) if d.resources else None
        "health_check": d.health_check if d.health_check else None
        "security_context": d.security_context if d.security_context else None
        "env_vars": [_render_env_var(e) for e in d.env_vars]
        "platform": d.platform if d.platform else None
        "ports": d.ports
        "cluster_rbac": _render_cluster_rbac(d.cluster_rbac) if d.cluster_rbac else None
    }
}

_render_vm_docker = lambda d: VMDocker -> any {
    {
        "type": "vm-docker"
        "ssh_host": d.ssh_host
        "image": d.image
        "tag": d.tag if d.tag else None
        "deploy_cmd": d.deploy_cmd if d.deploy_cmd else None
        "rollback_cmd": d.rollback_cmd if d.rollback_cmd else None
        "health_cmd": d.health_cmd if d.health_cmd else None
        "env_file": d.env_file if d.env_file else None
    }
}

_render_compose = lambda d: Compose -> any {
    {
        "type": "compose"
        "compose_file": d.compose_file
        "service": d.service if d.service else None
        "env_file": d.env_file if d.env_file else None
    }
}

_render_build_only = lambda d: BuildOnly -> any {
    {
        "type": "build-only"
        "build_variants": [_render_build_variant(bv) for bv in d.build_variants]
    }
}

# Dispatch on the populated deploy's discriminator. Called only when
# `d` is not Undefined / None. Five branches: host / cluster
# (K8sCluster) / vm-docker / compose / build-only.
_render_deploy_populated = lambda d: any -> any {
    _render_host_deploy(d) if d.type == "host" else \
        (_render_k8s_cluster(d) if d.type == "cluster" else \
        (_render_vm_docker(d) if d.type == "vm-docker" else \
        (_render_compose(d) if d.type == "compose" else _render_build_only(d))))
}

# Top-level deploy dispatcher. An unset deploy projects as null
# (consumer should treat that as `BuildOnly { build_variants = [] }`
# equivalent — just build).
_render_deploy = lambda d: any -> any {
    _render_deploy_populated(d) if d else None
}

_render_service = lambda s: Service -> any {
    {
        "name": s.name
        "image": s.image if s.image else None
        "command": s.command
        "env_vars": [_render_env_var(e) for e in s.env_vars]
        "deploy": _render_deploy(s.deploy)
    }
}

_render_operator = lambda o: Operator -> any {
    {
        "name": o.name
        "image": o.image
        "replicas": o.replicas
        "platform": o.platform if o.platform else None
        "crds": o.crds
        "cluster_rbac": _render_cluster_rbac(o.cluster_rbac)
        "leader_election": o.leader_election
        "env_vars": [_render_env_var(e) for e in o.env_vars]
    }
}

_render_frontend = lambda f: Frontend -> any {
    {
        "name": f.name
        "type": f.type
        "path": f.path
        "dev_runner": f.dev_runner
        "port": f.port
        "env_file": f.env_file if f.env_file else None
    }
}

_render_config_map = lambda cm: ConfigMap -> any {
    {
        "name": cm.name
        "namespace": cm.namespace if cm.namespace else None
        "data": cm.data
        "labels": cm.labels
        "annotations": cm.annotations
    }
}

_render_cronjob = lambda c: CronJob -> any {
    {
        "name": c.name
        "schedule": c.schedule if c.schedule else None
        "image": c.image
        "platform": c.platform if c.platform else None
        "command": c.command
        "env_vars": [_render_env_var(e) for e in c.env_vars]
    }
}

# Project a Bundle to the stable JSON contract. Consumed by forge CLI.
render = lambda bundle: Bundle -> any {
    {
        "services": [_render_service(s) for s in bundle.services]
        "operators": [_render_operator(o) for o in bundle.operators]
        "frontends": [_render_frontend(f) for f in bundle.frontends]
        "cronjobs": [_render_cronjob(c) for c in bundle.cronjobs]
        "config_maps": [_render_config_map(cm) for cm in bundle.config_maps]
    }
}

# ---------------------------------------------------------------------------
# Kubernetes manifest render.
#
# Walks the cluster-targeted entities and emits the k8s manifest list.
# Skips host-mode services and BuildOnly services — those live outside
# the cluster manifest boundary.
# ---------------------------------------------------------------------------

schema RenderEnv:
    """The environment-level knobs the manifest renderer needs."""
    namespace: str
    image_registry: str = ""
    image_tag: str = "latest"
    network_policies: bool = True

_render_namespace = lambda env: RenderEnv -> any {
    {
        apiVersion = "v1"
        kind = "Namespace"
        metadata = {
            name = env.namespace
            labels = {
                "app.kubernetes.io/managed-by" = "forge"
                "pod-security.kubernetes.io/enforce" = "restricted"
                "pod-security.kubernetes.io/audit" = "restricted"
                "pod-security.kubernetes.io/warn" = "restricted"
            }
        }
    }
}

_render_cluster_service = lambda s: Service, env: RenderEnv -> [any] {
    svc_lib.render_k8s_service(s, env.namespace, env.image_registry, env.image_tag) \
        + rbac_lib.render_namespaced_rbac(s.name, env.namespace)
}

_render_operator_manifests = lambda o: Operator, env: RenderEnv -> [any] {
    svc_lib.render_operator(o, env.namespace, env.image_registry, env.image_tag) \
        + rbac_lib.render_cluster_rbac(o.name, env.namespace, o.cluster_rbac)
}

_render_cronjob_manifests = lambda c: CronJob, env: RenderEnv -> [any] {
    svc_lib.render_cronjob(c, env.namespace, env.image_registry, env.image_tag) \
        + rbac_lib.render_namespaced_rbac(c.name, env.namespace)
}

# Render a ConfigMap as a k8s manifest. The metadata.namespace
# defaults to env.namespace when the caller hasn't pinned one
# explicitly on the schema.
_render_config_map_manifest = lambda cm: ConfigMap, env: RenderEnv -> any {
    {
        apiVersion = "v1"
        kind = "ConfigMap"
        metadata = {
            name = cm.name
            namespace = cm.namespace if cm.namespace else env.namespace
            labels = {
                "app.kubernetes.io/managed-by" = "forge"
                "app.kubernetes.io/part-of" = env.namespace
            } | cm.labels
            if cm.annotations:
                annotations = cm.annotations
        }
        data = cm.data
    }
}

# Emit the full Kubernetes manifest list for a Bundle in a given env.
render_manifests = lambda bundle: Bundle, env: RenderEnv -> [any] {
    _cluster_services = [s for s in bundle.services if s.deploy and s.deploy.type == "cluster"]
    [_render_namespace(env)] \
        + [_render_config_map_manifest(cm, env) for cm in bundle.config_maps] \
        + bundle.additional_manifests \
        + [item for s in _cluster_services for item in _render_cluster_service(s, env)] \
        + [item for o in bundle.operators for item in _render_operator_manifests(o, env)] \
        + [item for c in bundle.cronjobs for item in _render_cronjob_manifests(c, env)] \
        + (netpol_lib.render_network_policies(env.namespace) if env.network_policies else [])
}
