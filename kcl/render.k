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

`Service.deploy: HostDeploy | K8sCluster | External | Compose | BuildOnly`.
The discriminator field (`type`) is declared ON each variant subschema,
so the JSON projection is self-describing — the consumer dispatches
on `deploy.type` directly. The render lambdas here mirror that
contract: one branch per discriminator value, always producing a
fully-populated dict so the JSON shape is stable.
"""

import .lib.services as svc_lib
import .lib.rbac as rbac_lib
import .lib.netpol as netpol_lib
import .lib.gateway as gw_lib

# ---------------------------------------------------------------------------
# JSON contract render — typed Bundle -> stable dict shape.
# ---------------------------------------------------------------------------

# Render an EnvVar to its stable dict form. Unset / empty-string
# defaults project as `null` so the consumer sees stable keys — EXCEPT
# `value`, which preserves an explicitly-set empty string (rendered as
# `""`, not `null`). Empty-string `value` is a legal disable sentinel
# downstream (e.g. RUNTIME_CLASS_NAME="" to skip runtimeClassName).
_render_env_var = lambda ev: EnvVar -> any {
    {
        "name": ev.name
        "value": ev.value if ev.value != Undefined else None
        "secret_ref": ev.secret_ref if ev.secret_ref else None
        "secret_key": ev.secret_key if ev.secret_key else None
        "config_map_ref": ev.config_map_ref if ev.config_map_ref else None
        "config_map_key": ev.config_map_key if ev.config_map_key else None
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
        "working_dir": d.working_dir if d.working_dir else None
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
        "resources": _render_resources(d.resources) if d.resources else None
        "health_check": d.health_check if d.health_check else None
        "security_context": d.security_context if d.security_context else None
        "env_vars": [_render_env_var(e) for e in d.env_vars]
        "platform": d.platform if d.platform else None
        "ports": d.ports
        "app_protocols": d.app_protocols if d.app_protocols else None
        "cluster_rbac": _render_cluster_rbac(d.cluster_rbac) if d.cluster_rbac else None
        "image_pull_secrets": d.image_pull_secrets if d.image_pull_secrets else None
    }
}

_render_external = lambda d: External -> any {
    {
        "type": "external"
        "deploy_cmd": d.deploy_cmd
        "rollback_cmd": d.rollback_cmd if d.rollback_cmd else None
        "health_cmd": d.health_cmd if d.health_cmd else None
        # build_cmd is the build-side mirror of deploy_cmd; the Go-side
        # parser (ExternalDeploy.BuildCmd) reads it for `forge build -t
        # external`. None when unset so existing External targets that
        # only deploy through forge stay unchanged.
        "build_cmd": d.build_cmd if d.build_cmd else None
        "env_file": d.env_file if d.env_file else None
        "env": d.env if d.env else {}
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
# (K8sCluster) / external / compose / build-only.
_render_deploy_populated = lambda d: any -> any {
    _render_host_deploy(d) if d.type == "host" else \
        (_render_k8s_cluster(d) if d.type == "cluster" else \
        (_render_external(d) if d.type == "external" else \
        (_render_compose(d) if d.type == "compose" else _render_build_only(d))))
}

# Top-level deploy dispatcher. An unset deploy projects as null
# (consumer should treat that as `BuildOnly { build_variants = [] }`
# equivalent — just build).
_render_deploy = lambda d: any -> any {
    _render_deploy_populated(d) if d else None
}

# build_of coerces a plain build dict (the shape components_gen.json
# emits and a per-env overlay authors) into the right TYPED Build subtype,
# dispatching on the `type` discriminator. This exists because KCL union
# coercion is structural-first: a ShellBuild dict ({type=shell, cmd=...})
# also satisfies GoBuild's required `cmd`, so a bare dict assigned to
# `build: GoBuild | DockerBuild | ShellBuild` wrongly commits to GoBuild
# and fails its `type == "go"` check. Routing the dict through this
# discriminator-aware constructor at the bridge (build = forge.build_of(
# c.build)) yields the exact subtype so the union match is unambiguous —
# the same way the deploy bridge passes typed K8sCluster / BuildOnly
# literals rather than bare dicts. Module-level (not a lambda body) so it
# may instantiate schema literals (the v0.11 lambda restriction doesn't
# apply here). An empty dict returns Undefined so `if c.build` stays the
# "no build declared" signal.
_build_go = lambda b: {str:any} -> GoBuild {
    GoBuild {
        cmd = b.cmd
        output_name = b.output_name if "output_name" in b else ""
        goos = b.goos if "goos" in b else ""
        goarch = b.goarch if "goarch" in b else ""
        ldflags = b.ldflags if "ldflags" in b else []
        tags = b.tags if "tags" in b else []
        flags = b.flags if "flags" in b else []
        env = b.env if "env" in b else {}
    }
}

_build_docker = lambda b: {str:any} -> DockerBuild {
    DockerBuild {
        output_name = b.output_name if "output_name" in b else ""
        dockerfile = b.dockerfile if "dockerfile" in b else "Dockerfile"
        platform = b.platform if "platform" in b else ""
        target = b.target if "target" in b else ""
        build_args = b.build_args if "build_args" in b else {}
    }
}

_build_shell = lambda b: {str:any} -> ShellBuild {
    ShellBuild {
        output_name = b.output_name if "output_name" in b else ""
        cmd = b.cmd
    }
}

build_of = lambda b: {str:any} -> any {
    _build_go(b) if (b and b.type == "go") else \
        (_build_docker(b) if (b and b.type == "docker") else \
        (_build_shell(b) if (b and b.type == "shell") else Undefined))
}

# Build union render. One branch per discriminator value (go / docker /
# shell), each returning a fully-populated dict so the Go consumer
# (ServiceEntity.Build) sees a stable key shape per type. Mirrors the
# deploy-union dispatch above exactly.
_render_go_build = lambda b: GoBuild -> any {
    {
        "type": "go"
        "output_name": b.output_name if b.output_name else None
        "cmd": b.cmd
        "goos": b.goos if b.goos else None
        "goarch": b.goarch if b.goarch else None
        "ldflags": b.ldflags
        "tags": b.tags
        "flags": b.flags
        "env": b.env if b.env else {}
    }
}

_render_docker_build = lambda b: DockerBuild -> any {
    {
        "type": "docker"
        "output_name": b.output_name if b.output_name else None
        "dockerfile": b.dockerfile
        "platform": b.platform if b.platform else None
        "target": b.target if b.target else None
        "build_args": b.build_args if b.build_args else {}
    }
}

_render_shell_build = lambda b: ShellBuild -> any {
    {
        "type": "shell"
        "output_name": b.output_name if b.output_name else None
        "cmd": b.cmd
    }
}

# Dispatch on the populated build's discriminator. Called only when `b`
# is not Undefined / None. An unset build projects as null — the Go side
# (build.go) then synthesizes the GoBuild { cmd = "./cmd/<name>" }
# default, so a hand-authored forge.Service that omits `build` still
# builds.
_render_build_populated = lambda b: any -> any {
    _render_go_build(b) if b.type == "go" else \
        (_render_docker_build(b) if b.type == "docker" else _render_shell_build(b))
}

_render_build = lambda b: any -> any {
    _render_build_populated(b) if b else None
}

_render_service = lambda s: Service -> any {
    {
        "name": s.name
        "image": s.image if s.image else None
        "command": s.command
        "env_vars": [_render_env_var(e) for e in s.env_vars]
        "deploy": _render_deploy(s.deploy)
        "build": _render_build(s.build)
        "build_cmd": s.build_cmd if s.build_cmd else None
        "build_cwd": s.build_cwd if s.build_cwd else None
        "build_env": s.build_env if s.build_env else {}
        "namespaced_rbac": s.namespaced_rbac if s.namespaced_rbac else None
        # Owned raw manifests project into the JSON contract too (null when
        # empty) so a consumer that reads `output` sees the same stable
        # key shape; the cluster manifest stream stamps + routes them.
        "manifests": s.manifests if s.manifests else None
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

_render_firebase_bundle_dir = lambda b: FirebaseBundleDir -> any {
    {
        "src": b.src
        "dest": b.dest
    }
}

_render_firebase_hosting = lambda d: FirebaseHosting -> any {
    {
        "type": "firebase"
        "project": d.project
        "site": d.site
        "target": d.target if d.target else None
        "public_dir": d.public_dir
        "base_path": d.base_path if d.base_path else None
        "bundle": [_render_firebase_bundle_dir(b) for b in d.bundle]
        "rewrites": d.rewrites if d.rewrites else []
    }
}

# Frontend deploy dispatcher. Only FirebaseHosting today; an unset deploy
# projects as null (consumer treats that as "dev-serve / build only,
# never auto-ship").
_render_frontend_deploy = lambda d: any -> any {
    _render_firebase_hosting(d) if d else None
}

_render_frontend = lambda f: Frontend -> any {
    {
        "name": f.name
        "type": f.type
        "path": f.path
        "dev_runner": f.dev_runner
        "port": f.port
        "env_file": f.env_file if f.env_file else None
        "env_vars": [_render_env_var(e) for e in f.env_vars]
        "deploy": _render_frontend_deploy(f.deploy)
    }
}

_render_gateway_listener = lambda l: GatewayListener -> any {
    {
        "name": l.name
        "port": l.port
        "protocol": l.protocol
        "path_prefix": l.path_prefix if l.path_prefix else None
    }
}

_render_gateway_tls = lambda t: GatewayTLS -> any {
    {
        "cert_issuer": t.cert_issuer if t.cert_issuer else None
        "secret_name": t.secret_name if t.secret_name else None
        "certmap": t.certmap if t.certmap else None
        "mode": t.mode
    }
}

_render_gateway_address = lambda a: GatewayAddress -> any {
    {
        "type": a.type
        "value": a.value
    }
}

_render_gateway = lambda g: Gateway -> any {
    {
        "name": g.name
        "gateway_class_name": g.gateway_class_name
        "host": g.host if g.host else None
        "tls": _render_gateway_tls(g.tls) if g.tls else None
        "listeners": [_render_gateway_listener(l) for l in g.listeners]
        "raw_policy": g.raw_policy if g.raw_policy else None
        "addresses": [_render_gateway_address(a) for a in g.addresses] if g.addresses else None
    }
}

_render_http_route = lambda r: HTTPRoute -> any {
    {
        "name": r.name
        "gateway": r.gateway
        "listener": r.listener
        "service": r.service
        "port": r.port
        "host": r.host if r.host else None
        "path": r.path if r.path else None
        "raw_policy": r.raw_policy if r.raw_policy else None
    }
}

_render_grpc_route = lambda r: GRPCRoute -> any {
    {
        "name": r.name
        "gateway": r.gateway
        "listener": r.listener
        "service": r.service
        "port": r.port
        "host": r.host if r.host else None
        "path": r.path if r.path else None
        "raw_policy": r.raw_policy if r.raw_policy else None
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

# RuntimeClass JSON projection — stable shape mirroring the schema.
# `overhead` / `scheduling` pass through as raw dicts (or null when
# unset) so the Go consumer can re-emit them into the k8s manifest
# without per-field translation.
_render_runtime_class = lambda rc: RuntimeClass -> any {
    {
        "name": rc.name
        "handler": rc.handler
        "overhead": rc.overhead if rc.overhead else None
        "scheduling": rc.scheduling if rc.scheduling else None
    }
}

# Render a Cluster to its stable dict form. Optional fields project as
# null (not omitted) so the Go consumer (ClusterEntity) sees stable
# keys; servers/agents always carry their (defaulted) int.
_render_cluster = lambda c: Cluster -> any {
    {
        "name": c.name
        "config": c.config if c.config else None
        "network": c.network if c.network else None
        "registry_mirror": c.registry_mirror if c.registry_mirror else None
        "servers": c.servers
        "agents": c.agents
        "api_port": c.api_port if c.api_port else None
    }
}

# Project a Bundle to the stable JSON contract. Consumed by forge CLI.
render = lambda bundle: Bundle -> any {
    {
        "clusters": [_render_cluster(c) for c in bundle.clusters]
        "services": [_render_service(s) for s in bundle.services]
        "operators": [_render_operator(o) for o in bundle.operators]
        "frontends": [_render_frontend(f) for f in bundle.frontends]
        "cronjobs": [_render_cronjob(c) for c in bundle.cronjobs]
        "config_maps": [_render_config_map(cm) for cm in bundle.config_maps]
        "gateways": [_render_gateway(g) for g in bundle.gateways]
        "http_routes": [_render_http_route(r) for r in bundle.http_routes]
        "grpc_routes": [_render_grpc_route(r) for r in bundle.grpc_routes]
        "runtime_classes": [_render_runtime_class(rc) for rc in bundle.runtime_classes]
        # The secret PROVIDER declaration rides alongside the entities so
        # the CLI can read WHERE secret values come from. Absent when
        # unset (no null garbage). Secret VALUES never enter this output —
        # only the declaration (type + path). NOT in render_manifests.
        if bundle.secret_provider:
            "secret_provider": bundle.secret_provider
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
    """The environment-level knobs the manifest renderer needs.

    `cluster` is the single source of truth for namespace + registry —
    the same K8sCluster ref a project's services use as their `deploy`
    block, re-passed here so the manifest renderer reads env-wide
    knobs from one place. Project code looks like:

        _dev_k8s = forge.K8sCluster {
            cluster   = "k3d-myapp"
            namespace = "myapp-dev"
            registry  = "localhost:5050"
            ports     = [8080]
        }

        _bundle = forge.Bundle { services = [ forge.Service { ..., deploy = _dev_k8s } ] }

        _env = forge.RenderEnv {
            cluster   = _dev_k8s
            image_tag = forge.image_tag("dev")
        }

    namespace/registry are read from `env.cluster.{namespace,registry}`
    internally. image_tag stays on RenderEnv because it's the per-build
    override (CI passes `-D image_tag=<sha>` per build).
    """
    cluster: K8sCluster
    image_tag: str = "latest"
    network_policies: bool = True

_render_namespace = lambda env: RenderEnv -> any {
    {
        apiVersion = "v1"
        kind = "Namespace"
        metadata = {
            name = env.cluster.namespace
            labels = {
                "app.kubernetes.io/managed-by" = "forge"
                "pod-security.kubernetes.io/enforce" = "restricted"
                "pod-security.kubernetes.io/audit" = "restricted"
                "pod-security.kubernetes.io/warn" = "restricted"
            }
        }
    }
}

# Env-wide image-pull Secrets (private registry). Declared once on the
# K8sCluster ref and applied to every workload's generated
# ServiceAccount. Defaults to [] (field omitted on the SA) when unset.
_pull_secrets = lambda env: RenderEnv -> [str] {
    env.cluster.image_pull_secrets if env.cluster.image_pull_secrets else []
}

_render_cluster_service = lambda s: Service, env: RenderEnv -> [any] {
    svc_lib.render_k8s_service(s, env.cluster.namespace, env.cluster.registry, env.image_tag) \
        + rbac_lib.render_namespaced_rbac(
            s.name,
            env.cluster.namespace,
            s.namespaced_rbac if s.namespaced_rbac else [],
            _pull_secrets(env),
        )
}

_render_operator_manifests = lambda o: Operator, env: RenderEnv -> [any] {
    svc_lib.render_operator(o, env.cluster.namespace, env.cluster.registry, env.image_tag) \
        + rbac_lib.render_cluster_rbac(o.name, env.cluster.namespace, o.cluster_rbac, _pull_secrets(env))
}

_render_cronjob_manifests = lambda c: CronJob, env: RenderEnv -> [any] {
    svc_lib.render_cronjob(c, env.cluster.namespace, env.cluster.registry, env.image_tag) \
        + rbac_lib.render_namespaced_rbac(c.name, env.cluster.namespace, [], _pull_secrets(env))
}

# Render a RuntimeClass as a node.k8s.io/v1 manifest. Cluster-scoped
# (no namespace). Forge labels every RuntimeClass with the standard
# managed-by/part-of so `kubectl get runtimeclasses -l
# app.kubernetes.io/managed-by=forge` is the canonical "what did
# forge create" query and the manifest-driven sweep can prune ones the
# Bundle no longer declares.
_render_runtime_class_manifest = lambda rc: RuntimeClass, env: RenderEnv -> any {
    {
        apiVersion = "node.k8s.io/v1"
        kind = "RuntimeClass"
        metadata = {
            name = rc.name
            labels = {
                "app.kubernetes.io/managed-by" = "forge"
                "app.kubernetes.io/part-of" = env.cluster.namespace
            }
        }
        handler = rc.handler
        if rc.overhead:
            overhead = rc.overhead
        if rc.scheduling:
            scheduling = rc.scheduling
    }
}

# Render a ConfigMap as a k8s manifest. The metadata.namespace
# defaults to env.cluster.namespace when the caller hasn't pinned one
# explicitly on the schema.
_render_config_map_manifest = lambda cm: ConfigMap, env: RenderEnv -> any {
    {
        apiVersion = "v1"
        kind = "ConfigMap"
        metadata = {
            name = cm.name
            namespace = cm.namespace if cm.namespace else env.cluster.namespace
            labels = {
                "app.kubernetes.io/managed-by" = "forge"
                "app.kubernetes.io/part-of" = env.cluster.namespace
            } | cm.labels
            if cm.annotations:
                annotations = cm.annotations
        }
        data = cm.data
    }
}

# Emit the full Kubernetes manifest list for a Bundle in a given env.
#
# RuntimeClasses come first so they exist in the cluster before any
# workload that references one (a Pod spec'ing `runtimeClassName =
# "gvisor"` would otherwise hit "RuntimeClass not found" while
# kube-apiserver applies the rest of the bundle). They're
# cluster-scoped (no namespace) so they survive namespace deletes —
# `forge deploy <env>` is the right owner.
render_manifests = lambda bundle: Bundle, env: RenderEnv -> [any] {
    _cluster_services = [s for s in bundle.services if s.deploy and s.deploy.type == "cluster"]
    # Only a service with an `image` renders a workload (Deployment +
    # Service + RBAC). An image-less cluster service is an INFRA service:
    # it contributes ONLY its owned `manifests` (below), no phantom
    # Deployment. This split is the declared-cluster-only model's no-build
    # infra-service shape — explicit (no image) rather than inferred.
    _workload_services = [s for s in _cluster_services if s.image]
    [_render_runtime_class_manifest(rc, env) for rc in bundle.runtime_classes] \
        + [_render_namespace(env)] \
        + [_render_config_map_manifest(cm, env) for cm in bundle.config_maps] \
        + bundle.additional_manifests \
        + [item for s in _workload_services for item in _render_cluster_service(s, env)] \
        + [item for s in _cluster_services for item in svc_lib.render_owned_manifests(s)] \
        + [item for o in bundle.operators for item in _render_operator_manifests(o, env)] \
        + [item for c in bundle.cronjobs for item in _render_cronjob_manifests(c, env)] \
        + [item for g in bundle.gateways for item in gw_lib.render_gateway(g, env.cluster.namespace)] \
        + [item for g in bundle.gateways for item in gw_lib.render_certificate(g, env.cluster.namespace)] \
        + [item for r in bundle.http_routes for item in gw_lib.render_http_route(r, bundle.gateways, env.cluster.namespace)] \
        + [item for r in bundle.grpc_routes for item in gw_lib.render_grpc_route(r, bundle.gateways, env.cluster.namespace)] \
        + (netpol_lib.render_network_policies(env.cluster.namespace) if env.network_policies else [])
}

# Emit ONLY the non-workload manifests of a Bundle: ConfigMaps,
# additional_manifests, RuntimeClasses, and the Gateway API ingress
# (Gateways + Certificates + HTTP/GRPC routes). The workload entities
# (services / operators / cronjobs) are NOT rendered here.
#
# This is the companion to forge.components.render_components: the
# deploy-as-data env main.k expands its workloads from
# components_gen.json (via render_components) and concatenates this
# helper for the env-owned support resources (the config the
# config_gen.k file projects, plus the project's ingress topology).
# Namespace + network policies are owned by render_components, so they
# are deliberately omitted here to avoid duplication.
render_support = lambda bundle: Bundle, env: RenderEnv -> [any] {
    # Owned manifests of any cluster services declared on the support
    # Bundle (the deploy-as-data path's home for image-less infra
    # services that pin env-level resources to a specific cluster). Stamped
    # with the owning service's app label so the deploy layer routes them.
    _support_cluster_services = [s for s in bundle.services if s.deploy and s.deploy.type == "cluster"]
    [_render_runtime_class_manifest(rc, env) for rc in bundle.runtime_classes] \
        + [_render_config_map_manifest(cm, env) for cm in bundle.config_maps] \
        + bundle.additional_manifests \
        + [item for s in _support_cluster_services for item in svc_lib.render_owned_manifests(s)] \
        + [item for g in bundle.gateways for item in gw_lib.render_gateway(g, env.cluster.namespace)] \
        + [item for g in bundle.gateways for item in gw_lib.render_certificate(g, env.cluster.namespace)] \
        + [item for r in bundle.http_routes for item in gw_lib.render_http_route(r, bundle.gateways, env.cluster.namespace)] \
        + [item for r in bundle.grpc_routes for item in gw_lib.render_grpc_route(r, bundle.gateways, env.cluster.namespace)]
}
