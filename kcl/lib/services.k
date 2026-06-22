"""
Forge KCL — per-Service k8s manifest builders.

Pure render functions that take a typed forge.Service with
`deploy = K8sCluster{...}` and emit Deployment + Service + (optional)
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

# Collapse an EnvVar list to one entry per name, LAST-occurrence-wins.
#
# The common authoring pattern is `cfg.APP_ENV + [ ...per-service overrides ]`
# (KCL `+` concatenates; the docs promise "KCL wins on conflict"). Without a
# dedup the rendered container env carries BOTH entries for an overridden key,
# and kubectl server-side apply REJECTS the Deployment: "duplicate entries for
# key [name=...]". Keep the last occurrence so a per-service override (e.g. a
# plain value) supersedes the base (e.g. an APP_ENV secretKeyRef) exactly the
# way the host-mode env layering already resolves it — one consistent rule
# across host and cluster.
_dedup_env_vars = lambda evs: [any] -> [any] {
    [
        evs[i] for i in range(len(evs))
        # include this entry only if no LATER entry shares its name
        if len([1 for j in range(i + 1, len(evs)) if evs[j].name == evs[i].name]) == 0
    ]
}

_env_source = lambda ev: any -> any {
    {
        name = ev.name
        # `ev.value != Undefined` preserves an explicitly-set empty
        # string (e.g. `value = ""` as a disable sentinel) while still
        # omitting `value` from the manifest when the field was never
        # set (secret_ref/config_map_ref-only entries).
        if ev.value != Undefined:
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

# Pick `imagePullPolicy` for a container based on the image tag.
#
# Default is `IfNotPresent` (the k8s default for tagged images), which
# is what we want for committed/clean SHA tags — the kubelet can cache
# the layer and skip the network round-trip.
#
# Two cases force `Always`:
#
#   1. Tag ends in `-dirty`. `forge build` appends `-dirty` to the
#      git-sha image tag whenever the worktree has uncommitted changes.
#      The SHA doesn't move while the worktree stays dirty, so each
#      rebuild lands a NEW image under the SAME `<sha>-dirty` tag.
#      With `IfNotPresent`, the kubelet sees the tag locally already
#      and runs the stale layer — `forge deploy` then reports
#      "rollout successful" while the cluster runs the previous binary.
#      `Always` forces a re-pull on every pod start, which restores
#      the dev iteration loop. (The long-term answer is content-
#      addressed tags — see `docs/proposals/image-content-addressed-tags.md`.)
#
#   2. Tag is `latest` (the implicit default when no `:tag` is set).
#      `:latest` is a moving target by definition; the standard k8s
#      behavior is to flip the default to `Always` for `:latest`. We
#      do the same explicitly so the policy is grep-able from the
#      rendered manifest.
#
# Only matches when the tag actually *ends* in `-dirty` — an image
# whose tag happens to contain "dirty" in the middle (e.g.
# `:dirty-feature-v1`) gets the `IfNotPresent` default.
_pull_policy = lambda image_ref: str -> str {
    # `image_ref` is the full reference, e.g. `localhost:5051/foo:abc1234-dirty`
    # or `ghcr.io/x/y:latest`. The tag is the substring AFTER the last
    # `:` AFTER the last `/` (the registry-port `:` is before the last
    # `/`, so the rightmost `/` then rightmost `:` lookup is safe).
    _last_slash = image_ref.rfind("/")
    _ref_tail = image_ref[_last_slash + 1:] if _last_slash >= 0 else image_ref
    _last_colon = _ref_tail.rfind(":")
    _tag = _ref_tail[_last_colon + 1:] if _last_colon >= 0 else "latest"
    "Always" if _tag.endswith("-dirty") or _tag == "latest" else "IfNotPresent"
}

# Build the pod-level `spec.volumes[*]` entry for one forge.Volume.
# empty_dir => `{name, emptyDir: {}}`; config_map / secret =>
# `{name, configMap|secret: {<name-key>: source}}`. The container-side
# mount is built separately by `_volume_mount`.
_volume_source = lambda v: any -> any {
    {
        name = v.name
        if v.type == "empty_dir":
            emptyDir = {}
        if v.type == "config_map":
            configMap = {name = v.source}
        if v.type == "secret":
            secret = {secretName = v.source}
    }
}

# Build the container `spec.containers[*].volumeMounts[*]` entry for one
# forge.Volume. `subPath` / `readOnly` are omitted when not set/false so
# the rendered manifest stays minimal.
_volume_mount = lambda v: any -> any {
    {
        name = v.name
        mountPath = v.mount_path
        if v.sub_path != Undefined:
            subPath = v.sub_path
        if v.read_only:
            readOnly = True
    }
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

# Render a Service whose deploy is K8sCluster. Returns
# [Deployment, Service]. Ingress is no longer rendered here — see
# kcl/lib/gateway.k for Gateway API resources owned by the project's
# top-level `deploy/kcl/ingress.k` file.
render_k8s_service = lambda svc: any, namespace: str, image_registry: str, image_tag: str -> [any] {
    _name = svc.name
    _labels = _managed_labels(_name)
    _deploy = svc.deploy
    _resources = _deploy.resources if _deploy.resources else _default_resources
    _image = _image_ref(svc.image, image_registry, image_tag)
    _ports = _deploy.ports if len(_deploy.ports) > 0 else [8080]
    # Optional writable / projected volumes (the readOnlyRootFilesystem
    # escape hatch). `svc.volumes` is None/Undefined when unset.
    _volumes = svc.volumes if svc.volumes else []

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
                    # Bind the per-Service ServiceAccount forge generates
                    # (render_namespaced_rbac). Without this the pod runs
                    # as `default` and the generated Role/RoleBinding —
                    # plus any imagePullSecrets on the SA — grant it
                    # nothing. Mirrors render_operator (~line 280).
                    serviceAccountName = _name
                    securityContext = _default_pod_security_context
                    containers = [
                        {
                            name = _name
                            image = _image
                            imagePullPolicy = _pull_policy(_image)
                            if svc.command:
                                command = svc.command
                            ports = [
                                {containerPort = p, protocol = "TCP", name = "port-${p}"}
                                for p in _ports
                            ]
                            resources = _resource_block(_resources)
                            if svc.env_vars:
                                env = [_env_source(e) for e in _dedup_env_vars(svc.env_vars)]
                            securityContext = _default_container_security_context
                            if _volumes:
                                volumeMounts = [_volume_mount(v) for v in _volumes]
                        }
                    ]
                    if _volumes:
                        volumes = [_volume_source(v) for v in _volumes]
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

    [_deployment, _service]
}

# Render a CronJob (or one-shot Job when schedule == "").
render_cronjob = lambda cj: any, namespace: str, image_registry: str, image_tag: str -> [any] {
    _name = cj.name
    _labels = _managed_labels(_name)
    _image = _image_ref(cj.image, image_registry, image_tag)
    _volumes = cj.volumes if cj.volumes else []
    _pod_spec = {
        # Bind the per-CronJob ServiceAccount forge generates
        # (_render_cronjob_manifests -> render_namespaced_rbac). Without
        # this the Job pod runs as `default`, which lacks the
        # imagePullSecrets forge attaches to the per-workload SA — so a
        # schedule=="" one-shot migrate Job ImagePullBackOffs on private
        # registries. Mirrors render_k8s_service (~line 171) and
        # render_operator (~line 286).
        serviceAccountName = _name
        restartPolicy = "OnFailure"
        securityContext = _default_pod_security_context
        containers = [
            {
                name = _name
                image = _image
                imagePullPolicy = _pull_policy(_image)
                if cj.command:
                    command = cj.command
                if cj.env_vars:
                    env = [_env_source(e) for e in _dedup_env_vars(cj.env_vars)]
                securityContext = _default_container_security_context
                if _volumes:
                    volumeMounts = [_volume_mount(v) for v in _volumes]
            }
        ]
        if _volumes:
            volumes = [_volume_source(v) for v in _volumes]
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
    _user_env = [_env_source(e) for e in _dedup_env_vars(op.env_vars)]
    _all_env = _user_env + _leader_env_dict
    _volumes = op.volumes if op.volumes else []
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
                            imagePullPolicy = _pull_policy(_image)
                            if _all_env:
                                env = _all_env
                            securityContext = _default_container_security_context
                            if _volumes:
                                volumeMounts = [_volume_mount(v) for v in _volumes]
                        }
                    ]
                    if _volumes:
                        volumes = [_volume_source(v) for v in _volumes]
                }
            }
        }
    }]
}
