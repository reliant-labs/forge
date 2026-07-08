"""
Forge KCL — component expansion (deploy-as-data normalization).

Pure render functions that take a typed forge.Component (Server /
Worker / Cron / Operator / Binary) and emit its Kubernetes resources.
This is where the denormalized component shape forge serializes to
`components_gen.json` gets normalized into k8s manifests — forge owns
ZERO of this; the schema hierarchy + these expanders own ALL of it.

Per kind:
  * Server   → Deployment + Service (named ports → containerPort/ServicePort)
  * Worker   → Deployment
  * Cron     → CronJob (or one-shot Job when schedule == "")
  * Operator → manager Deployment + cluster-scoped RBAC
  * Binary   → Deployment running command (["/app/<bin>"])

Lambdas take `any` for the component arg by convention — see the
package-identity note in `lib/services.k` for why KCL forces dynamic
field access here.
"""

import ..lib.rbac as rbac_lib
import ..lib.quantity as qty_lib

_managed_labels = lambda name: str -> {str: str} {
    {
        "app.kubernetes.io/name" = name
        "app.kubernetes.io/managed-by" = "forge"
        "app.kubernetes.io/part-of" = name
    }
}

_default_pod_security_context = {
    runAsNonRoot = True
    runAsUser = 65532
    runAsGroup = 65532
    seccompProfile = {type = "RuntimeDefault"}
}

_default_container_security_context = {
    runAsNonRoot = True
    runAsUser = 65532
    runAsGroup = 65532
    readOnlyRootFilesystem = True
    allowPrivilegeEscalation = False
    capabilities = {drop = ["ALL"]}
}

_default_resources = {
    cpu_request_millicores = 100
    cpu_limit_millicores = 500
    memory_request_bytes = 134217728    # 128Mi
    memory_limit_bytes = 536870912      # 512Mi
}

_image_ref = lambda image: str, image_registry: str, image_tag: str -> str {
    "${image_registry}/${image}:${image_tag}" if image_registry \
        else "${image}:${image_tag}"
}

_pull_policy = lambda image_ref: str -> str {
    _last_slash = image_ref.rfind("/")
    _ref_tail = image_ref[_last_slash + 1:] if _last_slash >= 0 else image_ref
    _last_colon = _ref_tail.rfind(":")
    _tag = _ref_tail[_last_colon + 1:] if _last_colon >= 0 else "latest"
    "Always" if _tag.endswith("-dirty") or _tag == "latest" else "IfNotPresent"
}

# k8s ADAPTER: neutral Resources (millicores + bytes) -> k8s quantity
# strings, via the shared quantity.k formatters.
_resource_block = lambda r: any -> any {
    {
        requests = {cpu = qty_lib.fmt_k8s_cpu(r.cpu_request_millicores), memory = qty_lib.fmt_k8s_mem(r.memory_request_bytes)}
        limits = {cpu = qty_lib.fmt_k8s_cpu(r.cpu_limit_millicores), memory = qty_lib.fmt_k8s_mem(r.memory_limit_bytes)}
    }
}

# Project an EnvVar (secret/config-map/value backed) to a k8s env entry.
_env_source = lambda ev: any -> any {
    {
        name = ev.name
        if ev.value != Undefined:
            value = ev.value
        if ev.secret_ref:
            valueFrom = {secretKeyRef = {name = ev.secret_ref, key = ev.secret_key if ev.secret_key else ev.name}}
        if ev.config_map_ref:
            valueFrom = {configMapKeyRef = {name = ev.config_map_ref, key = ev.config_map_key if ev.config_map_key else ev.name}}
    }
}

# Project the base `env` string map to k8s env entries (plain values),
# then append the typed per-env env_vars (secret/config-map backed).
_container_env = lambda c: any -> [any] {
    [{name = k, value = c.env[k]} for k in c.env] + [_env_source(e) for e in c.env_vars]
}

# Resolve the container image: the component's overlay image (set per
# env, typically the project image so shared-binary components all run
# one image), falling back to the component name.
_resolved_image = lambda c: any, env_registry: str, image_tag: str -> str {
    _img = c.image if c.image else c.name
    _reg = c.registry if c.registry else env_registry
    _image_ref(_img, _reg, image_tag)
}

# The exposed container ports for a component. Servers expose every
# declared port on the Service; all kinds open their ports as
# containerPorts. Falls back to 8080 when no ports are declared so a
# server is always reachable.
_container_ports = lambda c: any -> [any] {
    _ports = c.ports if len(c.ports) > 0 else [{name = "http", port = 8080, protocol = "tcp", expose = True}]
    [{containerPort = p.port, protocol = "TCP", name = p.name} for p in _ports]
}

# Shared Deployment builder. `service_account` is set for operators
# (which need a bound SA for cluster RBAC); empty otherwise.
_deployment = lambda c: any, namespace: str, image: str, ports: [any], extra_env: [any], service_account: str -> any {
    _name = c.name
    _labels = _managed_labels(_name)
    _resources = c.resources if c.resources else _default_resources
    {
        apiVersion = "apps/v1"
        kind = "Deployment"
        metadata = {name = _name, namespace = namespace, labels = _labels}
        spec = {
            replicas = c.replicas
            selector = {matchLabels = {"app.kubernetes.io/name" = _name}}
            template = {
                metadata = {labels = _labels}
                spec = {
                    if service_account:
                        serviceAccountName = service_account
                    securityContext = _default_pod_security_context
                    containers = [
                        {
                            name = _name
                            image = image
                            imagePullPolicy = _pull_policy(image)
                            if c.command:
                                command = c.command
                            if ports:
                                ports = ports
                            resources = _resource_block(_resources)
                            _all_env = _container_env(c) + extra_env
                            if _all_env:
                                env = _all_env
                            securityContext = _default_container_security_context
                        }
                    ]
                }
            }
        }
    }
}

# Server → Deployment + Service. Named ports become both
# containerPorts and ServicePorts.
render_server = lambda c: any, namespace: str, env_registry: str, image_tag: str -> [any] {
    _name = c.name
    _labels = _managed_labels(_name)
    _image = _resolved_image(c, env_registry, image_tag)
    _ports = c.ports if len(c.ports) > 0 else [{name = "http", port = 8080, protocol = "tcp", expose = True}]
    _container_port_list = [{containerPort = p.port, protocol = "TCP", name = p.name} for p in _ports]
    _deploy = _deployment(c, namespace, _image, _container_port_list, [], "")
    _service = {
        apiVersion = "v1"
        kind = "Service"
        metadata = {name = _name, namespace = namespace, labels = _labels}
        spec = {
            selector = {"app.kubernetes.io/name" = _name}
            ports = [{name = p.name, port = p.port, targetPort = p.port, protocol = "TCP"} for p in _ports]
        }
    }
    [_deploy, _service] + rbac_lib.render_namespaced_rbac(_name, namespace)
}

# Worker → Deployment only (no Service).
render_worker = lambda c: any, namespace: str, env_registry: str, image_tag: str -> [any] {
    _image = _resolved_image(c, env_registry, image_tag)
    [_deployment(c, namespace, _image, [], [], "")] \
        + rbac_lib.render_namespaced_rbac(c.name, namespace)
}

# Binary → Deployment running `command` (["/app/<bin>"]).
# Same shape as a worker; the distinct entrypoint comes from `command`.
render_binary = lambda c: any, namespace: str, env_registry: str, image_tag: str -> [any] {
    _image = _resolved_image(c, env_registry, image_tag)
    [_deployment(c, namespace, _image, [], [], "")] \
        + rbac_lib.render_namespaced_rbac(c.name, namespace)
}

# Cron → CronJob (or one-shot Job when schedule == "").
render_cron = lambda c: any, namespace: str, env_registry: str, image_tag: str -> [any] {
    _name = c.name
    _labels = _managed_labels(_name)
    _image = _resolved_image(c, env_registry, image_tag)
    _pod_spec = {
        restartPolicy = "OnFailure"
        containers = [
            {
                name = _name
                image = _image
                imagePullPolicy = _pull_policy(_image)
                if c.command:
                    command = c.command
                _env = _container_env(c)
                if _env:
                    env = _env
            }
        ]
    }
    _jobs = [{
        apiVersion = "batch/v1"
        kind = "CronJob"
        metadata = {name = _name, namespace = namespace, labels = _labels}
        spec = {
            schedule = c.schedule
            jobTemplate = {spec = {template = {metadata = {labels = _labels}, spec = _pod_spec}}}
        }
    }] if c.schedule else [{
        apiVersion = "batch/v1"
        kind = "Job"
        metadata = {name = _name, namespace = namespace, labels = _labels}
        spec = {template = {metadata = {labels = _labels}, spec = _pod_spec}}
    }]
    _jobs + rbac_lib.render_namespaced_rbac(_name, namespace)
}

# Operator → manager Deployment + cluster-scoped RBAC. The Deployment
# binds the operator's ServiceAccount; leader-election env is injected.
render_operator = lambda c: any, namespace: str, env_registry: str, image_tag: str -> [any] {
    _name = c.name
    _image = _resolved_image(c, env_registry, image_tag)
    _leader_env = [{name = "LEADER_ELECTION", value = "true"}] if c.leader_election else []
    _deploy = _deployment(c, namespace, _image, [], _leader_env, _name)
    # Operators get cluster RBAC with the default config-read rules
    # (rbac_lib adds them); component CRDs don't carry custom rules in
    # the denormalized shape, so pass an empty rule set.
    [_deploy] + rbac_lib.render_cluster_rbac(_name, namespace, {rules = []})
}

# Dispatch a single component on its kind. Each branch produces the
# kind's full manifest list. Unknown kinds produce nothing.
render_component = lambda c: any, namespace: str, env_registry: str, image_tag: str -> [any] {
    render_server(c, namespace, env_registry, image_tag) if c.kind == "server" else \
        (render_worker(c, namespace, env_registry, image_tag) if c.kind == "worker" else \
        (render_binary(c, namespace, env_registry, image_tag) if c.kind == "binary" else \
        (render_cron(c, namespace, env_registry, image_tag) if c.kind == "cron" else \
        (render_operator(c, namespace, env_registry, image_tag) if c.kind == "operator" else []))))
}
