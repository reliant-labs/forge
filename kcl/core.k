"""
Forge KCL — agnostic-core env primitives (ADDITIVE; see FORGE_KCL_IDIOMS.md §3).

This file introduces the target-agnostic core's model of a workload's
environment: a MAP keyed by variable name (`{str: EnvSource}`), plus the
k8s-adapter projection (`env_project`) that turns that map into the
list-shaped `[EnvVar]` the k8s renderer already consumes.

Why a MAP is the correct core (and `[EnvVar]` is a k8s wire artifact)
=====================================================================
k8s `env:` is a LIST `[{name, value/valueFrom}]`, so duplicate names are a
hard server-side-apply error — which is the ONLY reason forge models env as
`[EnvVar]` and needs `env_merge` + the hidden `_dedup_env_vars` to guard the
`base + overrides` list-`+` footgun (kcl/base.k, kcl/lib/services.k). A MAP
cannot hold two of a key, so:

  * duplicate names are STRUCTURALLY IMPOSSIBLE — the dedup footgun cannot
    occur by construction, so no `env_merge` is needed in the core;
  * authors COMPOSE with native KCL map-merge `base | overrides`
    (last-wins) — no O(n²) comprehension, no helper;
  * a single projection (`env_project`) is the ONLY place a list is
    produced — list-dedup legitimately lives there, as an adapter concern,
    because the map already guarantees uniqueness.

`{str: EnvSource}` is the authoring shape. `env_project` is the k8s
adapter's projection into the EXISTING `EnvVar` schema — so its output flows
through the unchanged render pipeline (`_render_env_var` for the JSON
contract; `_dedup_env_vars` + `_env_source` for k8s manifests) with zero
renderer changes. This is ADDITIVE: `EnvVar` and `env_merge` are untouched.

NOTE (ADDITIVE): control-plane and other consumers keep authoring
`env_vars: [EnvVar]` + `env_merge` until they migrate onto `{str: EnvSource}`
+ `env_project` separately. Both shapes coexist on the `forge` surface.
"""

import .lib.quantity as qty_lib

schema EnvSource:
    """A target-neutral, single-channel env value — the MAP value in the
    agnostic core's `env: {str: EnvSource}`.

    Mirrors `EnvVar`'s three projection channels, but SEMANTIC (no k8s wire
    shape) and single-value-carrying (the NAME is the map key, not a field):

      * `value = "..."`      — an inline literal. Empty string is a LEGAL
                               value (mirrors EnvVar: the check treats
                               "explicitly empty" as the value channel being
                               set — `value != Undefined`, NOT `if value`),
                               so `value = ""` renders `{name, value: ""}`.
      * `from_secret = {name, key}` — read from a Secret. `name` is the
                               Secret name, `key` the key within it. Projects
                               to EnvVar.secret_ref / secret_key (k8s
                               secretKeyRef).
      * `from_config = {name, key}` — read from a ConfigMap. `name` is the
                               ConfigMap name, `key` the key within it.
                               Projects to EnvVar.config_map_ref /
                               config_map_key (k8s configMapKeyRef).

    EXACTLY ONE channel must be set — a malformed source (zero channels, or
    two) is a render-time CHECK FAILURE, not a silently-dropped var. This is
    the map-shape's analogue of EnvVar's own "must set a source" check.
    """
    value?: str
    from_secret?: {str: str}
    from_config?: {str: str}

    check:
        # EXACTLY ONE channel. `value != Undefined` (not `value`) so an
        # explicit empty-string value counts as the value channel being set
        # — same empty-value semantics as EnvVar.
        len([_c for _c in [value != Undefined, from_secret != Undefined, from_config != Undefined] if _c]) == 1, \
            "EnvSource must set EXACTLY ONE channel: value (empty string OK) | from_secret | from_config"
        from_secret == Undefined or ("name" in from_secret and "key" in from_secret), \
            "EnvSource.from_secret must carry both 'name' and 'key' (e.g. {name = \"my-secret\", key = \"password\"})"
        from_config == Undefined or ("name" in from_config and "key" in from_config), \
            "EnvSource.from_config must carry both 'name' and 'key' (e.g. {name = \"my-config\", key = \"log-level\"})"

# Alias for lambda-body schema instantiation (see schema.k header — KCL
# v0.11 accepts BARE-identifier schema literals inside lambda bodies).
_EnvVarForProject = EnvVar

# env_project(m) — the k8s ADAPTER's projection: the agnostic-core env MAP
# `{str: EnvSource}` -> the list-shaped `[EnvVar]` the k8s renderer already
# consumes (kcl/lib/services.k `_env_source`, kcl/render.k `_render_env_var`).
#
# This is the ONLY place a list is produced from the core env map, and it is
# where list-shaped concerns (deterministic ORDER, the map->list flattening)
# legitimately live — an adapter concern. It SUBSUMES `env_merge`'s last-wins
# dedup: the map key is the var name, so uniqueness is already guaranteed and
# no dedup pass is needed (the output is dedup-free by construction).
#
# ORDER is deterministic — sorted by key (env-var NAME) — so the rendered
# container `env:` block is stable across renders regardless of KCL's map
# iteration order. (k8s env order is not semantically meaningful; a stable
# order keeps diffs clean.)
#
# Channel mapping (mirrors _env_source's k8s projection exactly):
#   value       -> EnvVar.value
#   from_secret -> EnvVar.secret_ref (name) + secret_key (key)  [secretKeyRef]
#   from_config -> EnvVar.config_map_ref (name) + config_map_key (key)  [configMapKeyRef]
#
# Native map-merge is how authors COMPOSE before projecting:
#   _env = _base_env | { "LOG_LEVEL" = {value = "debug"} }   # last-wins, no helper
#   env_vars = forge.env_project(_env)
env_project = lambda m: {str: EnvSource} -> [EnvVar] {
    [
        _EnvVarForProject {
            name = _k
            if m[_k].value != Undefined:
                value = m[_k].value
            if m[_k].from_secret != Undefined:
                secret_ref = m[_k].from_secret.name
                secret_key = m[_k].from_secret.key
            if m[_k].from_config != Undefined:
                config_map_ref = m[_k].from_config.name
                config_map_key = m[_k].from_config.key
        }
        for _k in sorted([_key for _key in m])
    ]
}

# ===========================================================================
# The ONE user-facing workload schema — target-AGNOSTIC (FORGE_KCL_IDIOMS.md).
# ===========================================================================
# `forge.Service` is what a project author writes: a target-agnostic
# description of a workload with ZERO Kubernetes vocabulary (no Deployment,
# no `env:` list, no `secretKeyRef`, no FQDN, no `containerPort`, no deploy
# union). Kubernetes is not *the* output — it is the FIRST adapter: the render
# pipeline projects this onto the demoted, k8s-shaped `K8sWorkload`
# (kcl/schema.k) via `_service_to_k8s` INTERNALLY, so the user never touches
# the adapter. A compose / Nomad / ECS adapter would project the SAME core
# differently.
#
# The old k8s-shaped `forge.Service` was renamed `forge.K8sWorkload` and
# demoted to adapter-internal plumbing; this schema takes over the
# `forge.Service` name as the single authoring surface.

schema EnvFrom:
    """A WHOLE secret / configmap imported into the env — the agnostic-core
    analogue of k8s `envFrom` (bulk-import case).

    Where `EnvSource` names ONE variable's single source (the map key is the
    var name), `EnvFrom` bulk-imports EVERY key of a whole secret or configmap
    as env vars — the abstract, target-neutral form of "load all of this
    secret/configmap into the environment".

      * `secret = "..."` — import every key of this Secret. Adapter: k8s
                           `envFrom.secretRef`; compose `env_file`.
      * `config = "..."` — import every key of this ConfigMap. Adapter:
                           k8s `envFrom.configMapRef`; compose inline env.
      * `prefix`         — optional string prepended to EVERY imported key
                           name (k8s `envFrom[].prefix`). Omit for none.

    EXACTLY ONE of secret / config must be set — a malformed EnvFrom (zero
    or both) is a CHECK FAILURE, mirroring EnvSource's exactly-one-channel
    rule.

    PRECEDENCE (adapter-preserved): an explicit `env` entry ALWAYS wins over
    a same-named key imported via `env_from`. On k8s this is native semantics
    — a container `env` entry overrides any `envFrom` value of the same name —
    so the k8s adapter emits both and k8s resolves the precedence.
    """
    secret?: str
    config?: str
    prefix?: str

    check:
        len([_c for _c in [secret != Undefined, config != Undefined] if _c]) == 1, \
            "EnvFrom must set EXACTLY ONE of: secret | config (a whole Secret or ConfigMap name)"

schema K8sOverrides:
    """The k8s ADAPTER's escape hatch — a target-NAMESPACED block on an
    agnostic `Service` (`svc.k8s`) carrying the narrow tail of k8s-only
    knobs the portable core deliberately does NOT model (leader election,
    cluster RBAC, CRDs stay on the k8s-native Operator/K8sWorkload; those
    don't port to compose). A Service with NO `k8s` block is fully
    portable; the block is where non-portability is made EXPLICIT and
    lintable (FORGE_KCL_IDIOMS.md §9).

    Applied BY the adapter (`_service_to_k8s`), NOT as a post-render merge:
    the adapter projects each field onto the demoted `K8sWorkload` it emits
    (which render_k8s_service then threads into the Deployment), so the
    override is a structural, by-name splice — never blind list-surgery on
    an already-rendered manifest.

    TIERED — typed common knobs first, `raw_manifests` as the ultimate
    floor:

    Tier 1 — typed, discoverable knobs for the common power-user needs:
      * `service_account` — bind an EXISTING out-of-band ServiceAccount
                            (e.g. one with a cloud IAM workload-identity
                            annotation) instead of the forge-generated
                            per-service SA.
      * `namespaced_rbac` — extra namespaced PolicyRule dicts appended to
                            the service's generated Role (same shape as
                            Operator.cluster_rbac.rules): controller-runtime
                            leases, in-namespace reads beyond config.
      * `annotations`     — pod-template annotations (GKE backend-config /
                            service-mesh / prometheus scrape hints).
      * `node_selector`   — pin the pod to a node pool (the control-plane
                            kata daemon-pool `reliant.dev/runtime=kata` case).
      * `tolerations`     — tolerate the matching node-pool taint
                            (`reliant.dev/runtime=kata:NoSchedule`).

    Tier 2 — the ULTIMATE floor, reached only for the genuinely exotic:
      * `raw_manifests`   — arbitrary k8s manifests OWNED by this service,
                            rendered verbatim and stamped with the service's
                            app label so the deploy layer routes them to the
                            service's cluster. Today's control-plane
                            `extra_manifests` (GKE HealthCheckPolicy, PDB,
                            LimitRange). Always available so no k8s
                            capability is ever unreachable — but the LAST
                            resort, not the only one.
    """
    service_account?: str
    namespaced_rbac?: [{str: any}]
    annotations?: {str: str}
    node_selector?: {str: str}
    tolerations?: [{str: any}]
    raw_manifests?: [any]

schema Service:
    """The ONE user-facing workload spec — TARGET-AGNOSTIC.

    ZERO k8s vocabulary. Every field is a target-agnostic fact about the
    workload; how each is realized (a k8s Deployment+Service, a compose
    service, …) is the ADAPTER's job (`_service_to_k8s`, run inside the render
    pipeline). Place these in `Bundle.workloads`; render_manifests / render
    project them onto `K8sWorkload` for you.

    Fields (all target-agnostic):
      * `name`     — workload identity. Adapter: k8s object name / compose
                     service key.
      * `image`    — container image (bare name or fully-qualified ref); the
                     adapter resolves registry/tag.
      * `command`  — the entrypoint argv. Omit to use the image's entrypoint.
      * `args`     — arguments appended after `command`. Kept SEPARATE in the
                     core (command = what to run, args = how); the k8s adapter
                     folds them into one argv. Omit for none.
      * `env`      — the environment as a MAP keyed by var NAME
                     (`{str: EnvSource}`) — the ONLY env authoring shape. A map
                     makes duplicate keys structurally impossible (no
                     `env_merge`) and merges with native `|`; the adapter
                     projects it to `[EnvVar]` via `env_project`.
      * `env_from` — WHOLE secrets / configmaps bulk-imported into the env
                     (see EnvFrom). Adapter: k8s `envFrom`. Explicit `env` wins
                     over `env_from` on a name collision. Omit for none.
      * `ports`    — the TCP ports the workload listens on, as plain ints.
                     Adapter: k8s Service ports + container ports. Omit / none.
      * `replicas` — desired instance count. Adapter: k8s Deployment replicas.
      * `resources`— compute requests/limits (forge.Resources). Omit to accept
                     the adapter's default.
      * `build`    — HOW this service's image is produced (GoBuild /
                     DockerBuild / ShellBuild — the polymorphic build union).
                     Target-AGNOSTIC: you build an image regardless of the
                     deploy target, so building is orthogonal to the k8s
                     adapter. Omit to accept forge's default
                     `go build ./cmd/<name>`.
      * `volumes`  — an agnostic volume model (forge.Volume: name + source
                     kind empty_dir|config_map|secret|pvc + mount path). The
                     adapter projects each to a k8s pod volume + container
                     volumeMount. The empty_dir case is also the writable
                     escape for the restricted-PSA readOnlyRootFilesystem
                     baseline. Omit for none.
      * `k8s`      — the k8s adapter ESCAPE HATCH (K8sOverrides): a
                     target-namespaced, TIERED block of k8s-only knobs
                     (service_account / namespaced_rbac / annotations /
                     node_selector / tolerations, then `raw_manifests` as
                     the floor). A Service with NO `k8s` block is fully
                     portable; the block is applied BY the adapter, so
                     non-portability is explicit and localized. Omit for a
                     fully-agnostic service.
    """
    name: str
    image: str
    command?: [str]
    args?: [str]
    env: {str: EnvSource} = {}
    env_from?: [EnvFrom]
    ports?: [int]
    replicas?: int = 1
    resources?: Resources
    build?: GoBuild | DockerBuild | ShellBuild
    volumes?: [Volume]
    k8s?: K8sOverrides

    check:
        name, "Service.name is required"
        image, "Service.image is required (a container image name or ref)"
        replicas > 0, "Service.replicas must be positive"

# Aliases for lambda-body schema instantiation (KCL v0.11 — see schema.k
# header). `_service_to_k8s` instantiates these inside its lambda body.
_K8sWorkloadForAdapter = K8sWorkload
_EnvFromSourceForAdapter = EnvFromSource

# _service_to_k8s — the k8s ADAPTER, run INSIDE the render pipeline (never by
# the user). Projects one agnostic `Service` onto the demoted k8s-shaped
# `K8sWorkload`, targeting the env-wide `ClusterTarget` (the SSOT for
# cluster/namespace/registry/…). The result is a valid K8sWorkload with a
# K8sCluster deploy block that flows through the UNCHANGED renderer.
#
#   Service.image      -> K8sWorkload.image        (renderer resolves reg/tag)
#   Service.command \
#   Service.args        -> K8sWorkload.command      (folded: command + args)
#   Service.env (map)   -> K8sWorkload.env_vars     via env_project (map->[EnvVar])
#   Service.env_from    -> K8sWorkload.env_from     (core EnvFrom -> EnvFromSource)
#   Service.ports       -> K8sCluster.ports
#   Service.replicas    -> K8sCluster.replicas
#   Service.resources   -> K8sCluster.resources     (same forge.Resources type)
#   Service.build       -> K8sWorkload.build         (same build union, pass-through)
#   Service.volumes     -> K8sWorkload.volumes       (same forge.Volume type)
#
# The k8s escape hatch (`Service.k8s` : K8sOverrides) is applied HERE, by
# the adapter — NOT as a post-render merge. Each typed knob projects onto a
# K8sWorkload field (which render_k8s_service threads into the Deployment /
# RBAC / owned-manifest stream), so the override is a structural by-name
# splice, and a Service with no `k8s` block projects byte-identically:
#   svc.k8s.service_account -> K8sWorkload.service_account
#   svc.k8s.namespaced_rbac -> K8sWorkload.namespaced_rbac
#   svc.k8s.annotations     -> K8sWorkload.pod_annotations
#   svc.k8s.node_selector   -> K8sWorkload.node_selector
#   svc.k8s.tolerations     -> K8sWorkload.tolerations
#   svc.k8s.raw_manifests   -> K8sWorkload.manifests  (owned + label-stamped)
#
# `target.deploy` is the ClusterTarget's derived per-service K8sCluster seed
# (pre-filled with cluster/namespace/registry/…); we `|`-overlay ONLY the
# per-service knobs this core spec carries. `resources` is overlaid only when
# the core sets it, so an unset core `resources` leaves the renderer's own
# default in place. Explicit `env` wins over `env_from` on a name collision
# (native k8s precedence — the renderer emits both untouched).
_service_to_k8s = lambda svc: Service, target: ClusterTarget -> K8sWorkload {
    _command = (svc.command if svc.command else []) + (svc.args if svc.args else [])

    _deploy = target.deploy | {
        ports = svc.ports if svc.ports else []
        replicas = svc.replicas
        if svc.resources:
            resources = svc.resources
    }

    _env_from = [
        _EnvFromSourceForAdapter {
            secret_ref = ef.secret if ef.secret else ""
            config_map_ref = ef.config if ef.config else ""
            prefix = ef.prefix if ef.prefix else ""
        }
        for ef in svc.env_from
    ] if svc.env_from else []

    _k8s = svc.k8s

    _K8sWorkloadForAdapter {
        name = svc.name
        image = svc.image
        command = _command
        env_vars = env_project(svc.env)
        env_from = _env_from
        deploy = _deploy
        if svc.build:
            build = svc.build
        if svc.volumes:
            volumes = svc.volumes
        # k8s escape hatch — applied by the adapter, namespaced under `k8s`.
        if _k8s:
            if _k8s.service_account:
                service_account = _k8s.service_account
            if _k8s.namespaced_rbac:
                namespaced_rbac = _k8s.namespaced_rbac
            if _k8s.annotations:
                pod_annotations = _k8s.annotations
            if _k8s.node_selector:
                node_selector = _k8s.node_selector
            if _k8s.tolerations:
                tolerations = _k8s.tolerations
            if _k8s.raw_manifests:
                manifests = _k8s.raw_manifests
    }
}

# ===========================================================================
# _service_to_compose — a SECOND adapter (SKETCH). The acid test of whether
# `forge.Service` is genuinely target-agnostic: project the SAME agnostic
# Service onto a docker-compose service dict, with ZERO reference to the k8s
# adapter or any k8s vocabulary. If the core is honest, every portable field
# falls out trivially and only the escape hatch is skipped.
# ===========================================================================
# This is a PROOF sketch, not a production compose feature — it renders one
# agnostic Service to a valid compose service and exposes any k8s-leak baked
# into the core. It has no render.k entry point (compose is not a deploy
# target here); the proof fixture calls it directly.
#
# Params are what a COMPOSE target needs — NOT a ClusterTarget:
#   * registry / tag — image resolution (target-agnostic: you resolve
#                      registry+tag regardless of target). Mirrors the k8s
#                      renderer so the SAME Service yields the SAME image ref.
#   * secrets_dir    — where the adapter DECIDES to place the `env_file`s it
#                      lowers `from_secret` / `env_from` onto. Purely a
#                      compose-adapter choice; the core never names a path.
#
# Field projection (the same agnostic Service, projected for compose):
#   Service.image           -> compose `image`  (registry/image:tag)
#   Service.command+args    -> compose `command` (folded argv, list form)
#   Service.env (map)       -> compose `environment` (a MAP — no dedup pass;
#                              the map key already guarantees uniqueness)
#   Service.env_from        -> compose `env_file`
#   Service.ports           -> compose `ports`   ("8080:8080")
#   Service.replicas        -> compose `deploy.replicas`
#   Service.resources       -> compose `deploy.resources` (limits/reservations)
#   Service.volumes         -> compose `volumes` ("source:target[:ro]")
#   Service.k8s             -> IGNORED. A compose adapter reads only a future
#                              target-namespaced `svc.compose` escape hatch,
#                              NEVER `svc.k8s`. That the escape hatch is
#                              target-NAMESPACED is exactly what keeps the core
#                              portable — the k8s-only tail cannot bleed into
#                              a non-k8s target because the adapter simply
#                              doesn't look at another target's namespace.
#
# The THREE env channels lower THREE different ways — this is the whole point:
# the core names WHAT (an inline value / a secret ref / a config ref); the
# adapter decides HOW for its target. Compose has no cluster Secret/ConfigMap,
# so:
#   value       -> inline literal in `environment`   (near-identity)
#   from_config -> a `${VAR}` interpolation in `environment` — compose
#                  substitutes it from its own process env / `.env` file at
#                  `up` time (there is no ConfigMap to reference)
#   from_secret -> the WHOLE owning secret is lowered to an `env_file` mount
#                  (secrets_dir/<secret>.env) — compose has no secretKeyRef
#                  inside `environment`
# Contrast the k8s adapter, where all three become a container `env` LIST
# entry (value / secretKeyRef / configMapKeyRef). Same abstract refs, two
# encodings — the proof that the core names the ref, the adapter encodes it.
_service_to_compose = lambda svc: Service, registry: str, tag: str, secrets_dir: str -> {str: any} {
    # command + args fold into one compose `command` argv (same fold as k8s).
    _command = (svc.command if svc.command else []) + (svc.args if svc.args else [])

    # env MAP -> compose `environment` MAP. The map STAYS a map — no
    # env_project, no list, no dedup. `value` -> inline; `from_config` ->
    # `${VAR}` interpolation (keyed by the env-var NAME).
    _env_inline = {_k: svc.env[_k].value for _k in svc.env if svc.env[_k].value != Undefined}
    _env_config = {_k: "$" + "{" + _k + "}" for _k in svc.env if svc.env[_k].from_config != Undefined}
    _environment = _env_inline | _env_config

    # `from_secret` -> `env_file`: lower each DISTINCT owning secret to one
    # env_file path. A secret carries many keys; the file supplies them all,
    # so we dedup secret NAMES (a Service using two keys of one secret yields
    # one env_file). Sorted for deterministic output.
    _secret_name_set = {svc.env[_k].from_secret.name: "" for _k in svc.env if svc.env[_k].from_secret != Undefined}
    _secret_files = [secrets_dir + "/" + _n + ".env" for _n in sorted([_key for _key in _secret_name_set])]
    # `env_from` (whole secret/configmap bulk import) -> env_file too.
    _from_files = [secrets_dir + "/" + (ef.secret if ef.secret else ef.config) + ".env" for ef in svc.env_from] if svc.env_from else []
    _env_files = _secret_files + _from_files

    # ports -> "host:container" strings (short form).
    _ports = [str(p) + ":" + str(p) for p in svc.ports] if svc.ports else []

    # volumes -> "source:target[:ro]". empty_dir has no source -> use the
    # volume name (a compose named volume); typed volumes use their source.
    _volumes = [
        (v.source if v.source else v.name) + ":" + v.mount_path + (":ro" if v.read_only else "")
        for v in svc.volumes
    ] if svc.volumes else []

    {
        "image" = registry + "/" + svc.image + ":" + tag
        if _command:
            "command" = _command
        if _environment:
            "environment" = _environment
        if _env_files:
            "env_file" = _env_files
        if _ports:
            "ports" = _ports
        if _volumes:
            "volumes" = _volumes
        "deploy" = {
            "replicas" = svc.replicas
            if svc.resources:
                # Resources holds NEUTRAL quantities (millicores + bytes); the
                # compose ADAPTER formats them for compose — millicores ->
                # decimal cores ("250" -> "0.25"), bytes -> raw byte int (a
                # valid compose deploy.resources memory value). No k8s dialect
                # leaks through (the leak the compose proof flagged is fixed).
                "resources" = {
                    "limits" = {"cpus" = qty_lib.fmt_compose_cpu(svc.resources.cpu_limit_millicores), "memory" = qty_lib.fmt_compose_mem(svc.resources.memory_limit_bytes)}
                    "reservations" = {"cpus" = qty_lib.fmt_compose_cpu(svc.resources.cpu_request_millicores), "memory" = qty_lib.fmt_compose_mem(svc.resources.memory_request_bytes)}
                }
        }
        # svc.k8s is DELIBERATELY unreferenced — see header.
    }
}
