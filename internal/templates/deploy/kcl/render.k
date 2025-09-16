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

_probe_spec = lambda hc: schema.HealthCheck -> any {
    {
        httpGet = {
            path = hc.http_path
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
                            livenessProbe = _probe_spec(app.health_check)
                            readinessProbe = {
                                httpGet = {
                                    path = "/readyz"
                                    port = app.health_check.http_port
                                }
                                initialDelaySeconds = app.health_check.initial_delay
                                periodSeconds = app.health_check.period
                                timeoutSeconds = app.health_check.timeout
                                failureThreshold = app.health_check.failure_threshold
                            }
                            startupProbe = {
                                httpGet = {
                                    path = "/healthz"
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
            if app.pdb.min_available is not None:
                minAvailable = app.pdb.min_available
            if app.pdb.max_unavailable is not None:
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

render_environment = lambda env: schema.Environment -> [any] {
    [_render_namespace(env)] + [
        item
        for _, app in env.applications
        for item in [
            _render_deployment(app, env)
            _render_service(app, env)
        ] + (
            [_render_ingress(app, env)] if app.ingress.enabled else []
        ) + (
            [_render_hpa(app, env)] if app.autoscaling.enabled else []
        ) + (
            [_render_pdb(app, env)] if app.pdb.enabled else []
        ) + (
            _render_rbac(app, env)
        )
    ] + (
        _render_network_policies(env) if env.network_policies else []
    )
}