"""
Forge Render Engine
Private lambdas that produce Kubernetes manifests from Application and Environment schemas.
"""

import .schema

_managed_labels = lambda app: schema.Application -> {str: str} {
    {
        "app.kubernetes.io/name" = app.name
        "app.kubernetes.io/managed-by" = "forge"
        "app.kubernetes.io/part-of" = app.name
    } | app.labels
}

_env_source = lambda ev: schema.EnvVar -> any {
    {
        name = ev.name
        if ev.value:
            value = ev.value
        if ev.secret_ref:
            valueFrom = {
                secretKeyRef = {
                    name = ev.secret_ref
                    key = ev.secret_key if ev.secret_key else ev.name
                }
            }
        if ev.config_map_ref:
            valueFrom = {
                configMapKeyRef = {
                    name = ev.config_map_ref
                    key = ev.config_map_key if ev.config_map_key else ev.name
                }
            }
    }
}

_container_security_context = lambda sc: schema.SecurityContext -> any {
    {
        runAsNonRoot = sc.run_as_non_root
        runAsUser = sc.run_as_user
        runAsGroup = sc.run_as_group
        readOnlyRootFilesystem = sc.read_only_root_filesystem
        allowPrivilegeEscalation = sc.allow_privilege_escalation
        capabilities = {drop = sc.drop_capabilities}
    }
}

_pod_security_context = lambda sc: schema.SecurityContext -> any {
    {
        runAsNonRoot = sc.run_as_non_root
        runAsUser = sc.run_as_user
        runAsGroup = sc.run_as_group
        seccompProfile = {type = sc.seccomp_profile}
    }
}

# Build a probe spec for a specific endpoint. `path` lets callers point the
# same shared probe timing at different endpoints (`/healthz` for liveness,
# `/readyz` for readiness).
_probe_spec = lambda hc: schema.HealthCheck, path: str -> any {
    {
        httpGet = {
            path = path
            port = hc.http_port
        }
        initialDelaySeconds = hc.initial_delay
        periodSeconds = hc.period
        timeoutSeconds = hc.timeout
        failureThreshold = hc.failure_threshold
    }
}

_render_deployment = lambda app: schema.Application, env: schema.Environment -> any {
    _labels = _managed_labels(app)
    {
        apiVersion = "apps/v1"
        kind = "Deployment"
        metadata = {
            name = app.name
            namespace = env.namespace
            labels = _labels
            if app.annotations:
                annotations = app.annotations
        }
        spec = {
            replicas = app.replicas
            selector = {matchLabels = {"app.kubernetes.io/name" = app.name}}
            strategy = {
                type = "RollingUpdate"
                rollingUpdate = {
                    maxUnavailable = "25%"
                    maxSurge = "25%"
                }
            }
            template = {
                metadata = {
                    labels = _labels
                    if app.annotations:
                        annotations = app.annotations
                }
                spec = {
                    if app.service_account_name:
                        serviceAccountName = app.service_account_name
                    securityContext = _pod_security_context(app.security_context)
                    if app.init_containers:
                        initContainers = [
                            {
                                name = ic.name
                                image = ic.image
                                if ic.command:
                                    command = ic.command
                                if ic.env:
                                    env = [_env_source(e) for e in ic.env]
                            }
                            for ic in app.init_containers
                        ]
                    containers = [
                        {
                            name = app.name
                            image = "${env.image_registry}/${app.image}:${env.image_tag}" if env.image_registry else "${app.image}:${env.image_tag}"
                            if app.command:
                                command = app.command
                            if app.args:
                                args = app.args
                            ports = [
                                {
                                    containerPort = p.target_port
                                    protocol = p.protocol
                                    name = p.name
                                }
                                for p in app.ports
                            ]
                            resources = {
                                requests = {
                                    cpu = app.resources.cpu_request
                                    memory = app.resources.memory_request
                                }
                                limits = {
                                    cpu = app.resources.cpu_limit
                                    memory = app.resources.memory_limit
                                }
                            }
                            if app.env_vars:
                                env = [_env_source(e) for e in app.env_vars]
                            securityContext = _container_security_context(app.security_context)
                            livenessProbe = _probe_spec(app.health_check, app.health_check.liveness_path)
                            readinessProbe = _probe_spec(app.health_check, app.health_check.readiness_path)
                            startupProbe = {
                                httpGet = {
                                    path = app.health_check.liveness_path
                                    port = app.health_check.http_port
                                }
                                failureThreshold = 30
                                periodSeconds = 2
                            }
                        }
                    ]
                }
            }
        }
    }
}

_render_service = lambda app: schema.Application, env: schema.Environment -> any {
    {
        apiVersion = "v1"
        kind = "Service"
        metadata = {
            name = app.name
            namespace = env.namespace
            labels = _managed_labels(app)
        }
        spec = {
            selector = {"app.kubernetes.io/name" = app.name}
            ports = [
                {
                    name = p.name
                    port = p.port
                    targetPort = p.target_port
                    protocol = p.protocol
                }
                for p in app.ports
            ]
        }
    }
}

_render_ingress = lambda app: schema.Application, env: schema.Environment -> any {
    {
        apiVersion = "networking.k8s.io/v1"
        kind = "Ingress"
        metadata = {
            name = "${app.name}-ingress"
            namespace = env.namespace
            labels = _managed_labels(app)
            annotations = {
                "cert-manager.io/cluster-issuer" = app.ingress.cert_manager_issuer
            }
        }
        spec = {
            if app.ingress.tls:
                tls = [
                    {
                        hosts = [app.ingress.host]
                        secretName = "${app.name}-tls"
                    }
                ]
            rules = [
                {
                    host = app.ingress.host
                    http = {
                        paths = [
                            {
                                path = p["path"]
                                pathType = p["path_type"] if "path_type" in p else "Prefix"
                                backend = {
                                    service = {
                                        name = app.name
                                        port = {number = app.ports[0].port}
                                    }
                                }
                            }
                            for p in app.ingress.paths
                        ]
                    }
                }
            ]
        }
    }
}

_render_hpa = lambda app: schema.Application, env: schema.Environment -> any {
    {
        apiVersion = "autoscaling/v2"
        kind = "HorizontalPodAutoscaler"
        metadata = {
            name = "${app.name}-hpa"
            namespace = env.namespace
            labels = _managed_labels(app)
        }
        spec = {
            scaleTargetRef = {
                apiVersion = "apps/v1"
                kind = "Deployment"
                name = app.name
            }
            minReplicas = app.autoscaling.min_replicas
            maxReplicas = app.autoscaling.max_replicas
            metrics = [
                {
                    type = "Resource"
                    resource = {
                        name = "cpu"
                        target = {
                            type = "Utilization"
                            averageUtilization = app.autoscaling.cpu_target
                        }
                    }
                }
                {
                    type = "Resource"
                    resource = {
                        name = "memory"
                        target = {
                            type = "Utilization"
                            averageUtilization = app.autoscaling.memory_target
                        }
                    }
                }
            ]
        }
    }
}

_render_pdb = lambda app: schema.Application, env: schema.Environment -> any {
    {
        apiVersion = "policy/v1"
        kind = "PodDisruptionBudget"
        metadata = {
            name = "${app.name}-pdb"
            namespace = env.namespace
            labels = _managed_labels(app)
        }
        spec = {
            selector = {matchLabels = {"app.kubernetes.io/name" = app.name}}
            if app.pdb.min_available != Undefined:
                minAvailable = app.pdb.min_available
            if app.pdb.max_unavailable != Undefined:
                maxUnavailable = app.pdb.max_unavailable
        }
    }
}

_render_network_policies = lambda env: schema.Environment -> [any] {
    [
        {
            apiVersion = "networking.k8s.io/v1"
            kind = "NetworkPolicy"
            metadata = {
                name = "default-deny"
                namespace = env.namespace
            }
            spec = {
                podSelector = {}
                policyTypes = ["Ingress", "Egress"]
            }
        }
        {
            apiVersion = "networking.k8s.io/v1"
            kind = "NetworkPolicy"
            metadata = {
                name = "allow-dns"
                namespace = env.namespace
            }
            spec = {
                podSelector = {}
                policyTypes = ["Egress"]
                egress = [
                    {
                        to = [
                            {namespaceSelector = {matchLabels = {"kubernetes.io/metadata.name" = "kube-system"}}}
                        ]
                        ports = [
                            {protocol = "UDP", port = 53}
                            {protocol = "TCP", port = 53}
                        ]
                    }
                ]
            }
        }
        {
            apiVersion = "networking.k8s.io/v1"
            kind = "NetworkPolicy"
            metadata = {
                name = "allow-internal"
                namespace = env.namespace
            }
            spec = {
                podSelector = {}
                policyTypes = ["Ingress", "Egress"]
                ingress = [
                    {from = [{podSelector = {}}]}
                ]
                egress = [
                    {to = [{podSelector = {}}]}
                ]
            }
        }
        {
            apiVersion = "networking.k8s.io/v1"
            kind = "NetworkPolicy"
            metadata = {
                name = "allow-ingress-controller"
                namespace = env.namespace
            }
            spec = {
                podSelector = {}
                policyTypes = ["Ingress"]
                ingress = [
                    {
                        from = [
                            {namespaceSelector = {matchLabels = {"kubernetes.io/metadata.name" = "ingress-nginx"}}}
                        ]
                    }
                ]
            }
        }
    ]
}

_render_rbac = lambda app: schema.Application, env: schema.Environment -> [any] {
    _sa_name = app.service_account_name if app.service_account_name else app.name
    [
        {
            apiVersion = "v1"
            kind = "ServiceAccount"
            metadata = {
                name = _sa_name
                namespace = env.namespace
                labels = _managed_labels(app)
            }
        }
        {
            apiVersion = "rbac.authorization.k8s.io/v1"
            kind = "Role"
            metadata = {
                name = "${app.name}-role"
                namespace = env.namespace
                labels = _managed_labels(app)
            }
            rules = [
                {
                    apiGroups = [""]
                    resources = ["configmaps", "secrets"]
                    verbs = ["get", "list", "watch"]
                }
            ]
        }
        {
            apiVersion = "rbac.authorization.k8s.io/v1"
            kind = "RoleBinding"
            metadata = {
                name = "${app.name}-rolebinding"
                namespace = env.namespace
                labels = _managed_labels(app)
            }
            subjects = [
                {
                    kind = "ServiceAccount"
                    name = _sa_name
                    namespace = env.namespace
                }
            ]
            roleRef = {
                apiGroup = "rbac.authorization.k8s.io"
                kind = "Role"
                name = "${app.name}-role"
            }
        }
    ]
}

# Render a ConfigMap. The metadata namespace defaults to the env's
# namespace when the caller didn't pin one explicitly on the schema.
_render_config_map = lambda cm: schema.ConfigMap, env: schema.Environment -> any {
    {
        apiVersion = "v1"
        kind = "ConfigMap"
        metadata = {
            name = cm.name
            namespace = cm.namespace if cm.namespace else env.namespace
            labels = {
                "app.kubernetes.io/managed-by" = "forge"
                "app.kubernetes.io/part-of" = env.name
            } | cm.labels
            if cm.annotations:
                annotations = cm.annotations
        }
        data = cm.data
    }
}

_render_namespace = lambda env: schema.Environment -> any {
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

# kcl's grammar treats a `]` followed by a newline (even inside parentheses)
# as end-of-statement when it appears directly in a lambda body, so the
# previous multi-line `] + (…)` continuation split into two statements and
# failed to parse. We factor the per-app list out into a helper lambda and
# use explicit `\` line continuations so the logical expression stays on a
# single statement while remaining readable.
_render_app = lambda app: schema.Application, env: schema.Environment -> [any] {
    [_render_deployment(app, env), _render_service(app, env)] \
        + ([_render_ingress(app, env)] if app.ingress.enabled else []) \
        + ([_render_hpa(app, env)] if app.autoscaling.enabled else []) \
        + ([_render_pdb(app, env)] if app.pdb.enabled else []) \
        + _render_rbac(app, env)
}

# Expand a MultiServiceApplication into a flat list of Applications —
# one per SubCommandService, all sharing the same image. Each child
# Application's `args` is the SubCommandService's args, and shared env
# vars from the parent are prepended (per-service env vars win on
# duplicate keys when the consumer flattens; KCL list concat preserves
# both, leaving the precedence to the runtime).
multi_service_apps = lambda mapp: schema.MultiServiceApplication -> {str: schema.Application} {
    {svc.name: schema.Application {
        name = svc.name
        image = mapp.image
        replicas = svc.replicas
        ports = svc.ports if svc.ports else [schema.ServicePort {}]
        resources = svc.resources
        env_vars = mapp.shared_env_vars + svc.env_vars
        health_check = svc.health_check
        security_context = svc.security_context
        ingress = svc.ingress
        autoscaling = svc.autoscaling
        pdb = svc.pdb
        init_containers = svc.init_containers
        labels = mapp.labels | svc.labels
        annotations = mapp.annotations | svc.annotations
        service_account_name = svc.service_account_name
        command = svc.command if svc.command else mapp.command
        args = svc.args
    } for svc in mapp.services}
}

render_environment = lambda env: schema.Environment -> [any] {
    [_render_namespace(env)] \
        + [_render_config_map(cm, env) for cm in env.config_maps] \
        + [item for _, app in env.applications for item in _render_app(app, env)] \
        + (_render_network_policies(env) if env.network_policies else [])
}