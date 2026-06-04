"""
Forge KCL — per-Service k8s manifest builders.

Pure render functions that take a typed forge.Service with
`deploy = K8sDeploy{...}` and emit Deployment + Service + (optional)
Ingress. Host-mode and BuildOnly services produce nothing here —
those paths are owned by the forge CLI runtime, not the cluster
manifest layer.

KCL package-identity note
=========================

KCL treats schemas declared in a parent package as DISTINCT types
from those reached via `import ..schema`. To keep the typed
entities user-facing under `forge.Service` etc. while still letting
this lib subdir consume them, we type the lambda parameters as
`any`. The caller (`render.k`) is responsible for passing
already-validated entities; field access here is dynamic.
"""

_managed_labels = lambda name: str -> {str: str} {
    {
        "app.kubernetes.io/name" = name
        "app.kubernetes.io/managed-by" = "forge"
        "app.kubernetes.io/part-of" = name
    }
}

_env_source = lambda ev: any -> any {
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

# Forge's secure baseline pod-level securityContext.
_default_pod_security_context = {
    runAsNonRoot = True
    runAsUser = 65532
    runAsGroup = 65532
    seccompProfile = {type = "RuntimeDefault"}
}

# Forge's secure baseline container-level securityContext.
_default_container_security_context = {
    runAsNonRoot = True
    runAsUser = 65532
    runAsGroup = 65532
    readOnlyRootFilesystem = True
    allowPrivilegeEscalation = False
    capabilities = {drop = ["ALL"]}
}

_image_ref = lambda image: str, image_registry: str, image_tag: str -> str {
    "${image_registry}/${image}:${image_tag}" if image_registry \
        else "${image}:${image_tag}"
}

_resource_block = lambda r: any -> any {
    {
        requests = {
            cpu = r.cpu_request
            memory = r.memory_request
        }
        limits = {
            cpu = r.cpu_limit
            memory = r.memory_limit
        }
    }
}

# Default resources when Service.deploy.resources is unset. Mirrors
# the forge.Resources schema defaults.
_default_resources = {
    cpu_request = "100m"
    cpu_limit = "500m"
    memory_request = "128Mi"
    memory_limit = "512Mi"
}

# Render a Service whose deploy is K8sDeploy. Returns
# [Deployment, Service] (+ [Ingress] when set).
render_k8s_service = lambda svc: any, namespace: str, image_registry: str, image_tag: str -> [any] {
    _name = svc.name
    _labels = _managed_labels(_name)
    _deploy = svc.deploy
    _resources = _deploy.resources if _deploy.resources else _default_resources
    _image = _image_ref(svc.image, image_registry, image_tag)
    _ports = _deploy.ports if len(_deploy.ports) > 0 else [8080]

    _deployment = {
        apiVersion = "apps/v1"
        kind = "Deployment"
        metadata = {
            name = _name
            namespace = namespace
            labels = _labels
        }
        spec = {
            replicas = _deploy.replicas
            selector = {matchLabels = {"app.kubernetes.io/name" = _name}}
            template = {
                metadata = {labels = _labels}
                spec = {
                    securityContext = _default_pod_security_context
                    containers = [
                        {
                            name = _name
                            image = _image
                            if svc.command:
                                command = svc.command
                            ports = [
                                {containerPort = p, protocol = "TCP", name = "port-${p}"}
                                for p in _ports
                            ]
                            resources = _resource_block(_resources)
                            if svc.env_vars:
                                env = [_env_source(e) for e in svc.env_vars]
                            securityContext = _default_container_security_context
                        }
                    ]
                }
            }
        }
    }

    _service = {
        apiVersion = "v1"
        kind = "Service"
        metadata = {
            name = _name
            namespace = namespace
            labels = _labels
        }
        spec = {
            selector = {"app.kubernetes.io/name" = _name}
            ports = [
                {name = "port-${p}", port = p, targetPort = p, protocol = "TCP"}
                for p in _ports
            ]
        }
    }

    _ingress_list = [{
        apiVersion = "networking.k8s.io/v1"
        kind = "Ingress"
        metadata = {
            name = "${_name}-ingress"
            namespace = namespace
            labels = _labels
            annotations = {
                "cert-manager.io/cluster-issuer" = _deploy.ingress.cert_manager_issuer
            } if _deploy.ingress.tls else {}
        }
        spec = {
            if _deploy.ingress.tls:
                tls = [
                    {
                        hosts = [_deploy.ingress.host]
                        secretName = "${_name}-tls"
                    }
                ]
            rules = [
                {
                    host = _deploy.ingress.host
                    http = {
                        paths = [
                            {
                                path = _deploy.ingress.path
                                pathType = "Prefix"
                                backend = {
                                    service = {
                                        name = _name
                                        port = {number = _ports[0]}
                                    }
                                }
                            }
                        ]
                    }
                }
            ]
        }
    }] if _deploy.ingress else []

    [_deployment, _service] + _ingress_list
}

# Render a CronJob (or one-shot Job when schedule == "").
render_cronjob = lambda cj: any, namespace: str, image_registry: str, image_tag: str -> [any] {
    _name = cj.name
    _labels = _managed_labels(_name)
    _image = _image_ref(cj.image, image_registry, image_tag)
    _pod_spec = {
        restartPolicy = "OnFailure"
        containers = [
            {
                name = _name
                image = _image
                if cj.command:
                    command = cj.command
                if cj.env_vars:
                    env = [_env_source(e) for e in cj.env_vars]
            }
        ]
    }
    [{
        apiVersion = "batch/v1"
        kind = "CronJob"
        metadata = {name = _name, namespace = namespace, labels = _labels}
        spec = {
            schedule = cj.schedule
            jobTemplate = {
                spec = {
                    template = {
                        metadata = {labels = _labels}
                        spec = _pod_spec
                    }
                }
            }
        }
    }] if cj.schedule else [{
        apiVersion = "batch/v1"
        kind = "Job"
        metadata = {name = _name, namespace = namespace, labels = _labels}
        spec = {
            template = {
                metadata = {labels = _labels}
                spec = _pod_spec
            }
        }
    }]
}

# Render an Operator. Same skeleton as a K8s Service Deployment but
# with leader-election env vars and cluster-scoped RBAC (handled in
# lib/rbac.k).
render_operator = lambda op: any, namespace: str, image_registry: str, image_tag: str -> [any] {
    _name = op.name
    _labels = _managed_labels(_name)
    _image = _image_ref(op.image, image_registry, image_tag)
    _leader_env_dict = [
        {name = "LEADER_ELECTION", value = "true"}
    ] if op.leader_election else []
    _user_env = [_env_source(e) for e in op.env_vars]
    _all_env = _user_env + _leader_env_dict
    [{
        apiVersion = "apps/v1"
        kind = "Deployment"
        metadata = {name = _name, namespace = namespace, labels = _labels}
        spec = {
            replicas = op.replicas
            selector = {matchLabels = {"app.kubernetes.io/name" = _name}}
            template = {
                metadata = {labels = _labels}
                spec = {
                    serviceAccountName = _name
                    securityContext = _default_pod_security_context
                    containers = [
                        {
                            name = _name
                            image = _image
                            if _all_env:
                                env = _all_env
                            securityContext = _default_container_security_context
                        }
                    ]
                }
            }
        }
    }]
}
