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

# Sibling module in this same lib package — the shared neutral-quantity
# formatters (Resources holds millicores + bytes; the k8s adapter formats).
import .quantity as qty_lib

_managed_labels = lambda name: str -> {str: str} {
    {
        "app.kubernetes.io/name" = name
        "app.kubernetes.io/managed-by" = "forge"
        "app.kubernetes.io/part-of" = name
    }
}

# Stamp the OWNING service's app.kubernetes.io/name onto an arbitrary raw
# manifest dict so the deploy layer can route it to that service's declared
# cluster (declared-cluster-only routing — there is no primary-cluster
# heuristic). Used by render_owned_manifests for the per-service
# `Service.manifests` field.
#
# The merge is deliberately non-destructive and kind-agnostic: it rebuilds
# `metadata` from whatever the manifest already carried (preserving name,
# namespace, annotations, ownerReferences, …) and merges the forge-managed
# labels UNDER any labels the user already set, so a user label always wins
# on key conflict. This works for ANY object — namespaced or cluster-scoped
# (Namespace, CRD, ClusterRole, ClusterIssuer) — because every k8s object
# has a metadata block and metadata.labels is universally legal.
#
# Label precedence (`_managed_labels(name) | _existing_labels`) mirrors how
# _render_config_map_manifest already merges forge defaults under user
# labels: the stamp is the DEFAULT owner, an explicit user-set
# app.kubernetes.io/name (rare) wins and routes the manifest to whatever
# the user named — explicit intent over the forge default. Re-rendering is
# idempotent (the stamp equals the existing label on the second pass).
#
# KCL v0.11 note: the body instantiates no schema literals — only raw-dict
# operations (`|`) and member access — so it is legal inside a lambda body.
_stamp_owner_label = lambda manifest: any, name: str -> any {
    _existing_meta = manifest.metadata if manifest.metadata else {}
    _existing_labels = _existing_meta.labels if _existing_meta.labels else {}
    manifest | {
        metadata = _existing_meta | {
            labels = _managed_labels(name) | _existing_labels
        }
    }
}

# Render a service's OWNED raw manifests (the per-service `Service.manifests`
# field), each stamped with the service's app.kubernetes.io/name so the
# deploy layer routes them to THIS service's declared cluster. Pure
# pass-through of the user's manifest dicts + the ownership label.
#
# This is the load-bearing mechanism for declared-cluster-only routing:
#   * a workload service may attach its CRD / extra RBAC here — those land
#     on the same cluster as the workload;
#   * an IMAGE-LESS "infra service" (a forge.Service with `manifests` + a
#     `deploy = K8sCluster{...}` and no image) declares env-level / ownerless
#     resources (Namespace, gateways, ClusterIssuers, bootstrap) onto a
#     specific cluster — no global render, no primary heuristic.
render_owned_manifests = lambda svc: any -> [any] {
    [_stamp_owner_label(m, svc.name) for m in (svc.manifests if svc.manifests else [])]
}

# Collapse an EnvVar list to one entry per name, LAST-occurrence-wins.
#
# This is the render-time SAFETY NET: it runs unconditionally on every
# service / cronjob / operator env list regardless of how the author
# assembled it, so a raw `base + overrides` concatenation (or any other
# source of a duplicate name) never reaches kubectl as a duplicate key —
# server-side apply REJECTS the Deployment: "duplicate entries for key
# [name=...]". Keep the last occurrence so a per-service override (e.g. a
# plain value) supersedes the base (e.g. an APP_ENV secretKeyRef) exactly
# the way the host-mode env layering already resolves it — one consistent
# rule across host and cluster.
#
# The PUBLIC, author-facing form of this same rule is `forge.env_merge(base,
# overrides)` (kcl/base.k) — projects SHOULD assemble env lists with that
# (explicit, discoverable, last-wins) rather than relying on this hidden
# net. This lambda is byte-for-byte the same merge logic as env_merge, so
# env_merge's output is already deduped and passing it through here is a
# no-op (idempotent). Retained as defense-in-depth for hand-rolled `+`.
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
        # Downward API: a field path ("metadata.name", "status.podIP",
        # "spec.nodeName") -> valueFrom.fieldRef.fieldPath. `field_ref`
        # defaults "" so this is emitted only when the EnvSource used the
        # from_field channel.
        if ev.field_ref:
            valueFrom = {
                fieldRef = {
                    fieldPath = ev.field_ref
                }
            }
    }
}

# Project one EnvFromSource onto a k8s container `envFrom[]` entry —
# `secretRef`/`configMapRef` + optional `prefix`. The EnvFromSource check
# guarantees exactly one of secret_ref/config_map_ref is set.
_env_from_source = lambda ef: any -> any {
    {
        if ef.secret_ref:
            secretRef = {name = ef.secret_ref}
        if ef.config_map_ref:
            configMapRef = {name = ef.config_map_ref}
        if ef.prefix:
            prefix = ef.prefix
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

# Pod-level securityContext with the agnostic SecurityOverrides
# (K8sWorkload.security_override) applied OVER the restricted-PSA default.
# `sec` is None/Undefined when the workload set no `security` block, in which
# case the default is returned UNCHANGED (byte-identical to before). Each set
# field overrides via dict-merge (`|`, right-wins). fsGroup is pod-ONLY (k8s
# has no container fsGroup). Body is raw-dict ops only — legal in a lambda.
_pod_security_context = lambda sec: any -> any {
    _default_pod_security_context | ({
        if sec.run_as_non_root != Undefined:
            runAsNonRoot = sec.run_as_non_root
        if sec.run_as_user != Undefined:
            runAsUser = sec.run_as_user
        if sec.run_as_group != Undefined:
            runAsGroup = sec.run_as_group
        if sec.fs_group != Undefined:
            fsGroup = sec.fs_group
    } if sec else {})
}

# Container-level securityContext with SecurityOverrides applied OVER the
# restricted-PSA default (see _pod_security_context). run_as_user /
# run_as_group are set here too (the container value WINS over the pod value
# in k8s, so a NATS UID-1000 override must land at the container level to take
# effect). read_only_root_fs and privileged are container-only knobs.
_container_security_context = lambda sec: any -> any {
    _default_container_security_context | ({
        if sec.run_as_non_root != Undefined:
            runAsNonRoot = sec.run_as_non_root
        if sec.run_as_user != Undefined:
            runAsUser = sec.run_as_user
        if sec.run_as_group != Undefined:
            runAsGroup = sec.run_as_group
        if sec.read_only_root_fs != Undefined:
            readOnlyRootFilesystem = sec.read_only_root_fs
        if sec.privileged != Undefined:
            privileged = sec.privileged
    } if sec else {})
}

# Build one k8s probe dict from an agnostic HealthProbe. `http_path` wins ->
# httpGet; else `tcp` -> tcpSocket. `default_port` (the service's first port)
# is used when the probe pins no port. The SAME dict is used for BOTH
# livenessProbe and readinessProbe (the 80/20 shape). Raw-dict body only —
# legal in a lambda.
_probe = lambda p: any, default_port: int -> any {
    _pt = p.port if p.port else default_port
    {
        if p.http_path != Undefined:
            httpGet = {path = p.http_path, port = _pt}
        if p.http_path == Undefined and p.tcp:
            tcpSocket = {port = _pt}
        initialDelaySeconds = p.initial_delay_seconds
        periodSeconds = p.period_seconds
        timeoutSeconds = p.timeout_seconds
        failureThreshold = p.failure_threshold
    }
}

# Render one projected sidecar / init Container (the neutral dict
# `_project_container` emits: {name, image, command, env_vars, ports}) into a
# full k8s container. Mirrors the primary container's own treatment: image
# ref resolution (registry/tag/digest), pull policy, containerPorts, env via
# _env_source (deduped), and the restricted-PSA container securityContext.
# Sidecars/init do NOT take the workload's `security_override` — that governs
# the primary/pod posture; extra containers keep the secure default (override
# per-sidecar is out of scope for the 80/20 shape).
_render_extra_container = lambda c: any, image_registry: str, image_tag: str, image_digests: {str:str} -> any {
    _img = _image_ref(c.image, image_registry, image_tag, image_digests)
    {
        name = c.name
        image = _img
        imagePullPolicy = _pull_policy(_img)
        if c.command:
            command = c.command
        if c.ports:
            ports = [
                {containerPort = p, protocol = "TCP", name = "port-${p}"}
                for p in c.ports
            ]
        if c.env_vars:
            env = [_env_source(e) for e in _dedup_env_vars(c.env_vars)]
        securityContext = _default_container_security_context
    }
}

# Does `image` already carry its OWN tag or digest — `name:tag` or
# `name@sha256:...` — so the env/service tag must NOT be appended?
#
# The disambiguation mirrors _pull_policy: a registry-port colon
# (`localhost:5050/foo`) is BEFORE the last `/`, so a tag colon is the
# rightmost `:` AFTER the rightmost `/`. A digest is any `@` in the ref.
_image_has_tag_or_digest = lambda image: str -> bool {
    _last_slash = image.rfind("/")
    _tail = image[_last_slash + 1:] if _last_slash >= 0 else image
    ("@" in image) or (":" in _tail)
}

# Build the full image reference for a container.
#
#   1. If `image` already pins its own tag/digest (`foo:1.2.3`,
#      `ghcr.io/x/y@sha256:...`), pass it through VERBATIM — only
#      prefixing the registry when one is set and the image has no
#      registry host of its own. The caller-supplied tag is ignored:
#      an explicitly-pinned image wins.
#   2. Else, if a PER-IMAGE digest was captured for THIS image name in
#      `image_digests` (`{<image-name>: "sha256:..."}`), pin
#      `<registry>/<image>@sha256:…` — the immutable, content-addressed
#      deploy reference `forge deploy` emits when the build state captured
#      a pushed digest for this image. The map is keyed on the BARE image
#      name (`reliant`, `control-plane`, `workspace-base`), so two services
#      that share an image both resolve to that image's OWN digest, and a
#      service whose image differs resolves to ITS image's digest — never a
#      single env-wide digest stamped onto every service. A digest can't go
#      stale and can't be re-pointed, so the node-cache / re-tag-didn't-take
#      class of bug is impossible. Images absent from the map (vendored
#      litellm/nats/postgres/temporal, or any build that captured no digest)
#      fall through to the tag, unchanged.
#   3. Otherwise append `:image_tag` (the per-service pin or the
#      env-wide tag, resolved by the caller).
_image_ref = lambda image: str, image_registry: str, image_tag: str, image_digests: {str:str} -> str {
    _pinned = _image_has_tag_or_digest(image)
    _digest = image_digests[image] if (image in image_digests) else ""
    "${image_registry}/${image}" if (_pinned and image_registry) \
        else (image if _pinned \
        else ("${image_registry}/${image}@${_digest}" if (_digest and image_registry) \
        else ("${image}@${_digest}" if _digest \
        else ("${image_registry}/${image}:${image_tag}" if image_registry \
        else "${image}:${image_tag}"))))
}

# Resolve the tag to stamp onto an entity's image: the entity's pinned
# `image_tag` when set (a no-op ShellBuild sibling that was never built
# under the env-wide git tag pins the tag it WAS pushed under), else the
# env-wide tag. `entity` is any schema carrying an optional `image_tag`
# field (Service / CronJob / Operator), all of which declare it so the
# read is always safe.
_effective_image_tag = lambda entity: any, env_tag: str -> str {
    entity.image_tag if entity.image_tag else env_tag
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
# `{name, configMap|secret: {<name-key>: source}}`; pvc =>
# `{name, persistentVolumeClaim: {claimName: source}}`. The container-side
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
        if v.type == "pvc":
            persistentVolumeClaim = {claimName = v.source}
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

# k8s ADAPTER: neutral Resources (millicores + bytes) -> the container
# resources block's k8s quantity strings. Formatters shared via quantity.k
# (same package) so every k8s call site emits identical dialect.
_resource_block = lambda r: any -> any {
    {
        requests = {
            cpu = qty_lib.fmt_k8s_cpu(r.cpu_request_millicores)
            memory = qty_lib.fmt_k8s_mem(r.memory_request_bytes)
        }
        limits = {
            cpu = qty_lib.fmt_k8s_cpu(r.cpu_limit_millicores)
            memory = qty_lib.fmt_k8s_mem(r.memory_limit_bytes)
        }
    }
}

# Default resources (neutral units) when Service.deploy.resources is unset.
# Mirrors the forge.Resources schema defaults: 100m/500m, 128Mi/512Mi.
_default_resources = {
    cpu_request_millicores = 100
    cpu_limit_millicores = 500
    memory_request_bytes = 134217728    # 128Mi
    memory_limit_bytes = 536870912      # 512Mi
}

# Render a Service whose deploy is K8sCluster. Returns
# [Deployment, Service]. Ingress is no longer rendered here — see
# kcl/lib/gateway.k for Gateway API resources owned by the project's
# top-level `deploy/kcl/ingress.k` file.
render_k8s_service = lambda svc: any, namespace: str, image_registry: str, image_tag: str, image_digests: {str:str} -> [any] {
    _name = svc.name
    _labels = _managed_labels(_name)
    _deploy = svc.deploy
    _resources = _deploy.resources if _deploy.resources else _default_resources
    _image = _image_ref(svc.image, image_registry, _effective_image_tag(svc, image_tag), image_digests)
    _ports = _deploy.ports if len(_deploy.ports) > 0 else [8080]
    # Optional per-port appProtocol map ({port: appProtocol}). Only
    # K8sCluster carries this; absent/empty => no port gets the field,
    # so the Service ports render byte-identically to before. Service-
    # port-only — never projected onto containerPorts.
    _app_protocols = _deploy.app_protocols if _deploy.app_protocols else {}
    # Optional writable / projected volumes (the readOnlyRootFilesystem
    # escape hatch). `svc.volumes` is None/Undefined when unset.
    _volumes = svc.volumes if svc.volumes else []
    # Pod-spec escape-hatch knobs (projected from Service.k8s / settable on
    # K8sWorkload). Each is absent-by-default so a workload setting none
    # renders byte-identically. `service_account` overrides the default
    # per-service SA name; the rest are conditionally emitted.
    _sa = svc.service_account if svc.service_account else _name
    _pod_annotations = svc.pod_annotations if svc.pod_annotations else {}
    _node_selector = svc.node_selector if svc.node_selector else {}
    _tolerations = svc.tolerations if svc.tolerations else []
    # securityContext override (agnostic Service.security -> K8sWorkload
    # .security_override). None when unset => the helpers return the restricted
    # -PSA default UNCHANGED, so a workload with no `security` is byte-identical.
    _sec = svc.security_override if svc.security_override else Undefined
    # Sidecar + init containers (projected [Container] dicts). Empty when
    # unset => the containers list is [primary] and no initContainers key is
    # emitted — a single-container workload renders byte-identically.
    _sidecars = svc.sidecars if svc.sidecars else []
    _init_containers = svc.init_containers if svc.init_containers else []
    # Health probe (agnostic Service.health -> K8sWorkload.health_probe). None
    # when unset => NO liveness/readiness emitted, byte-identical to before.
    _health = svc.health_probe if svc.health_probe else Undefined

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
                metadata = {
                    labels = _labels
                    # Pod annotations (GKE/mesh/prometheus hints) — omitted
                    # entirely when unset so the render stays minimal.
                    if _pod_annotations:
                        annotations = _pod_annotations
                }
                spec = {
                    # Bind the ServiceAccount: the per-Service SA forge
                    # generates (render_namespaced_rbac) by default, or an
                    # explicit `service_account` override (an existing
                    # out-of-band SA, e.g. one carrying a cloud
                    # workload-identity annotation). Without an SA the pod
                    # runs as `default` and the generated Role/RoleBinding —
                    # plus any imagePullSecrets on the SA — grant it
                    # nothing. Mirrors render_operator (~line 280).
                    serviceAccountName = _sa
                    # Node-pool pinning (the control-plane kata daemon-pool
                    # case) — nodeSelector + tolerations, each omitted when
                    # unset.
                    if _node_selector:
                        nodeSelector = _node_selector
                    if _tolerations:
                        tolerations = _tolerations
                    securityContext = _pod_security_context(_sec)
                    # Init containers run to completion before the primary
                    # starts. Emitted only when set, so single-container
                    # workloads render byte-identically (no initContainers key).
                    if _init_containers:
                        initContainers = [_render_extra_container(c, image_registry, _effective_image_tag(svc, image_tag), image_digests) for c in _init_containers]
                    # Primary container FIRST, then any sidecars appended. With
                    # no sidecars the list is [primary] — byte-identical.
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
                            # Whole Secret/ConfigMap bulk-imports (k8s
                            # envFrom). Emitted only when set, so services
                            # without env_from render byte-identically.
                            if svc.env_from:
                                envFrom = [_env_from_source(ef) for ef in svc.env_from]
                            # Health probe -> BOTH liveness + readiness (same
                            # config). Emitted only when `health` is set, so a
                            # workload without it renders probe-free as before.
                            if _health:
                                livenessProbe = _probe(_health, _ports[0])
                                readinessProbe = _probe(_health, _ports[0])
                            securityContext = _container_security_context(_sec)
                            if _volumes:
                                volumeMounts = [_volume_mount(v) for v in _volumes]
                        }
                    ] + [_render_extra_container(c, image_registry, _effective_image_tag(svc, image_tag), image_digests) for c in _sidecars]
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
                {
                    name = "port-${p}"
                    port = p
                    targetPort = p
                    protocol = "TCP"
                    # appProtocol is rendered only for ports listed in
                    # app_protocols (keyed by port-as-string); every other
                    # port omits the field and is byte-identical to the
                    # pre-appProtocol render.
                    if str(p) in _app_protocols:
                        appProtocol = _app_protocols[str(p)]
                }
                for p in _ports
            ]
        }
    }

    [_deployment, _service]
}

# Render a CronJob (or one-shot Job when schedule == "").
render_cronjob = lambda cj: any, namespace: str, image_registry: str, image_tag: str, image_digests: {str:str} -> [any] {
    _name = cj.name
    _labels = _managed_labels(_name)
    _image = _image_ref(cj.image, image_registry, _effective_image_tag(cj, image_tag), image_digests)
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
render_operator = lambda op: any, namespace: str, image_registry: str, image_tag: str, image_digests: {str:str} -> [any] {
    _name = op.name
    _labels = _managed_labels(_name)
    _image = _image_ref(op.image, image_registry, _effective_image_tag(op, image_tag), image_digests)
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
