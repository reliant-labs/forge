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
      * `from_field = "..."`  — the DOWNWARD API: read a field of the running
                               pod/container. The string is a k8s field path
                               ("metadata.name", "metadata.namespace",
                               "status.podIP", "spec.nodeName"). Projects to
                               EnvVar.field_ref (k8s valueFrom.fieldRef.
                               fieldPath). This is the channel controllers /
                               OTel resource-detection / pod-aware logging
                               need (POD_NAME, POD_NAMESPACE, POD_IP,
                               NODE_NAME). Compose has no analogue — the
                               compose adapter SKIPS from_field vars.

    EXACTLY ONE channel must be set — a malformed source (zero channels, or
    two) is a render-time CHECK FAILURE, not a silently-dropped var. This is
    the map-shape's analogue of EnvVar's own "must set a source" check.
    """
    value?: str
    from_secret?: {str: str}
    from_config?: {str: str}
    from_field?: str

    check:
        # EXACTLY ONE channel. `value != Undefined` (not `value`) so an
        # explicit empty-string value counts as the value channel being set
        # — same empty-value semantics as EnvVar.
        len([_c for _c in [value != Undefined, from_secret != Undefined, from_config != Undefined, from_field != Undefined] if _c]) == 1, \
            "EnvSource must set EXACTLY ONE channel: value (empty string OK) | from_secret | from_config | from_field"
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
# where list-shaped concerns (the map->list flattening) legitimately live —
# an adapter concern. It SUBSUMES `env_merge`'s last-wins dedup: the map key
# is the var name, so uniqueness is already guaranteed and no dedup pass is
# needed (the output is dedup-free by construction).
#
# ORDER is INSERTION order — the author's authoring order, preserved. KCL
# dicts (`{str: EnvSource}`) iterate in insertion order, and native `|`
# map-merge keeps base-key positions and appends new keys (verified
# empirically). This is LOAD-BEARING: k8s `env` supports dependent expansion
# — `VALUE: "$(EARLIER_VAR)/x"` resolves ONLY against entries appearing
# EARLIER in the list. A previous revision sorted by key here, which silently
# broke `$(VAR)` interpolation (a key `A = "$(Z)"` sorted before `Z` left
# `$(Z)` literal at runtime) AND was a regression vs `env_merge` (which keeps
# author order). Iterating in insertion order restores author control of
# ordering so `$(VAR)` works. Authors who want cross-referencing env just
# declare the referenced var EARLIER in the map literal.
#
# Channel mapping (mirrors _env_source's k8s projection exactly):
#   value       -> EnvVar.value
#   from_secret -> EnvVar.secret_ref (name) + secret_key (key)  [secretKeyRef]
#   from_config -> EnvVar.config_map_ref (name) + config_map_key (key)  [configMapKeyRef]
#   from_field  -> EnvVar.field_ref (k8s field path)            [fieldRef]
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
            if m[_k].from_field != Undefined:
                field_ref = m[_k].from_field
        }
        for _k in m
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

schema OperatorSpec:
    """The cluster-scoped OPERATOR tail of the k8s escape hatch
    (`svc.k8s.operator`) — see OPERATOR_AGNOSTIC.md.

    Setting this sub-block is what MARKS an agnostic `Service` as a
    Kubernetes OPERATOR (a controller-runtime manager that watches the API
    server): it is cluster-scoped, k8s-ONLY, and has NO compose analogue
    (the compose adapter SKIPS a Service that declares it — §5). It is the
    typed, first-class "this is a controller" predicate (`svc.k8s.operator
    != Undefined`) that lets forge keep ONE authoring surface
    (`forge.Service`) instead of forking a parallel top-level
    `forge.Operator` type — the portable 90% (image / ports / env /
    replicas / volumes) stays on the Service, and ONLY the k8s-bound tail
    lives here.

      * `cluster_rbac` — CLUSTER-scoped PolicyRule dicts, the SAME
                         `{apiGroups, resources, verbs}` shape as
                         `K8sOverrides.namespaced_rbac` (and the legacy
                         `Operator.cluster_rbac.rules`). The k8s adapter
                         routes these into a `ClusterRole` +
                         `ClusterRoleBinding` (via `render_cluster_rbac`),
                         named `<service>-clusterrole` — NOT the
                         namespaced `Role` a plain Service gets. Forge
                         still auto-adds the default config-read rule, so
                         authors supply only the CRD / lease / coordination
                         rules. Setting this is what flips the render path
                         from namespaced RBAC to cluster RBAC.
      * `crds`        — the CRD *kind names* this operator owns / serves
                        (e.g. `["Workspace"]`). This is a NAME LIST — the
                        bridge to `forge add crd` / the Go
                        `AddToScheme`/`SetupWithManager` wiring — NOT the
                        CRD manifest. The `CustomResourceDefinition` object
                        itself keeps riding `k8s.raw_manifests` (or the
                        bundle's `additional_manifests`), exactly as
                        control-plane already supplies its full kubebuilder
                        CRD; forge does NOT synthesize a CRD manifest from
                        this list.

    Leader election is deliberately NOT a field here: the pod's Lease is a
    pure-runtime concern (`operatorkit.Run`), driven by a normal
    `LEADER_ELECTION_ID` env entry plus a `coordination.k8s.io/leases` rule
    inside `cluster_rbac`.
    """
    cluster_rbac?: [{str: any}]
    crds?: [str]

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

      * `operator`        — the cluster-scoped OPERATOR sub-block
                            (OperatorSpec: cluster_rbac + crds). Setting it
                            MARKS this Service as a Kubernetes operator: the
                            adapter emits a cluster-scoped `ClusterRole` +
                            `ClusterRoleBinding` (not the namespaced `Role` a
                            plain Service gets), and the compose adapter
                            SKIPS the whole Service (an operator has no
                            docker-compose meaning). This is how a
                            controller-runtime workload joins the ONE
                            agnostic `Service` surface instead of forking a
                            parallel `forge.Operator` type — see OperatorSpec
                            and OPERATOR_AGNOSTIC.md.

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
    operator?: OperatorSpec
    annotations?: {str: str}
    node_selector?: {str: str}
    tolerations?: [{str: any}]
    raw_manifests?: [any]

schema SecurityOverrides:
    """Agnostic securityContext DELTAS on a `Service` (`svc.security`).

    The k8s adapter applies a restricted-PSA securityContext by DEFAULT
    (runAsNonRoot, runAsUser=65532, readOnlyRootFilesystem, drop:[ALL],
    RuntimeDefault seccomp) — see lib/services.k `_default_*_security_context`.
    That baseline is correct for a forge-built Go service but BLOCKS
    third-party infra images that need a different posture: NATS needs
    `fs_group` to write its JetStream store, temporal-auto-setup needs to run
    as root, litellm needs a writable root filesystem. Those workloads are
    raw Deployments TODAY only because the model had no securityContext knob.

    Every field is OPTIONAL: a field left unset keeps the restricted-PSA
    default, so a Service with NO `security` block renders BYTE-IDENTICALLY to
    before, and a Service that sets ONE field overrides only that field.

    The adapter applies each at the k8s level(s) where it is valid:
      * `run_as_user`     — pod + container `runAsUser` (a NATS UID 1000, a
                            root 0). Set at both so the container-level value
                            (which wins in k8s) actually takes effect.
      * `run_as_group`    — pod + container `runAsGroup`.
      * `fs_group`        — pod `fsGroup` ONLY (k8s has no container fsGroup) —
                            the volume-ownership knob JetStream needs.
      * `run_as_non_root` — pod + container `runAsNonRoot` (set False for a
                            root workload like temporal-auto-setup).
      * `read_only_root_fs` — container `readOnlyRootFilesystem` (set False
                            for litellm's prisma-generate write path).
      * `privileged`      — container `privileged` (container-only in k8s).
    """
    run_as_user?: int
    run_as_group?: int
    fs_group?: int
    run_as_non_root?: bool
    read_only_root_fs?: bool
    privileged?: bool

schema HealthProbe:
    """A MINIMAL agnostic health probe on a `Service` (`svc.health`).

    Closes the red-team's "every forge Deployment ships with NO probes" gap
    for the agnostic path — deliberately 80/20, NOT the full k8s probe matrix.
    ONE probe config renders as BOTH livenessProbe AND readinessProbe (the
    common case: most services use a single check for both). Separate
    liveness/readiness/startup knobs and exec/grpc handlers are OUT of scope —
    that is the `k8s.raw_manifests` escape hatch's job.

    Handler (pick one):
      * `http_path = "/healthz"` — an HTTP GET probe (k8s httpGet). Takes
                                   precedence if both are set.
      * `tcp = True`            — a TCP socket open probe (k8s tcpSocket) —
                                   for non-HTTP servers.
      * `port`                  — the port to probe. Defaults to the service's
                                   FIRST declared port when omitted.

    Timing (k8s defaults; tune only when needed):
      * `initial_delay_seconds` (0) / `period_seconds` (10) /
        `timeout_seconds` (1) / `failure_threshold` (3).

    Adapter: k8s emits liveness + readiness on the primary container; compose
    emits a `healthcheck`. A Service with no `health` renders NO probes (byte-
    identical to before).
    """
    http_path?: str
    tcp?: bool
    port?: int
    initial_delay_seconds: int = 0
    period_seconds: int = 10
    timeout_seconds: int = 1
    failure_threshold: int = 3

    check:
        http_path != Undefined or (tcp != Undefined and tcp), \
            "HealthProbe must set http_path (an HTTP GET path) or tcp = True"

schema Container:
    """A MINIMAL agnostic container — the shape of a `Service` sidecar or init
    container. Deliberately a subset of `Service`: the single `image` on a
    Service is THE primary-container happy path, and sidecars/init are the
    80/20 extension for the recurring multi-container needs (service-mesh /
    OTel-collector / cloud-sql-proxy sidecars; schema-migration / wait-for-dep
    init containers) WITHOUT complicating the single-container case.

    Fields (target-agnostic, mirroring the Service subset):
      * `name`    — container name (unique within the pod).
      * `image`   — container image (bare or fully-qualified); the adapter
                    resolves registry/tag exactly as it does the primary image.
      * `command` — entrypoint argv. Omit to use the image's entrypoint.
      * `args`    — args appended after command (folded into one argv, same as
                    the primary container).
      * `env`     — the container's env as a MAP keyed by var NAME
                    (`{str: EnvSource}`), projected via env_project like the
                    primary. Omit for none.
      * `ports`   — TCP ports the container listens on. Omit for none. (These
                    are containerPorts only — sidecars are not auto-exposed as
                    a k8s Service; declare Service.ports for that.)
    """
    name: str
    image: str
    command?: [str]
    args?: [str]
    env: {str: EnvSource} = {}
    ports?: [int]

    check:
        name, "Container.name is required"
        image, "Container.image is required (a container image name or ref)"

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
      * `health`    — a MINIMAL health probe (HealthProbe): one http_path/tcp
                     check rendered as BOTH liveness + readiness (the 80%
                     case). Closes the "no probes on any forge Deployment" gap
                     for the agnostic path. Omit for no probes (byte-identical
                     to before); the raw escape hatch covers exotic probe
                     shapes (startup, exec, grpc, distinct liveness/readiness).
      * `security`  — agnostic securityContext DELTAS (SecurityOverrides).
                     Every field optional; unset keeps forge's restricted-PSA
                     default (so a Service with no `security` renders
                     byte-identically). Overriding fs_group / run_as_user /
                     run_as_non_root / read_only_root_fs / privileged is what
                     lets third-party infra (NATS, temporal, litellm) fold
                     into the model instead of being raw Deployments. Omit for
                     the restricted default.
      * `sidecars`  — extra containers ([Container]) that run ALONGSIDE the
                     primary in the same pod (service-mesh / OTel-collector /
                     cloud-sql-proxy). The primary `image` stays THE happy
                     path; sidecars are the 80/20 add-on. Adapter: appended to
                     the pod's `containers` AFTER the primary. Omit for the
                     single-container case (renders exactly as before).
      * `init`      — init containers ([Container]) that run to completion
                     BEFORE the primary starts (schema migrations, wait-for-
                     dependency). Adapter: k8s `initContainers`. Omit for none.
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
    health?: HealthProbe
    security?: SecurityOverrides
    sidecars?: [Container]
    init?: [Container]
    k8s?: K8sOverrides

    check:
        name, "Service.name is required"
        image, "Service.image is required (a container image name or ref)"
        replicas > 0, "Service.replicas must be positive"

# Aliases for lambda-body schema instantiation (KCL v0.11 — see schema.k
# header). `_service_to_k8s` instantiates these inside its lambda body.
_K8sWorkloadForAdapter = K8sWorkload
_EnvFromSourceForAdapter = EnvFromSource

# _project_container — project one agnostic `Container` (a Service sidecar /
# init container) into the neutral dict render_k8s_service consumes: name,
# image, folded command+args argv, env projected to [EnvVar], and ports.
# Returns a RAW dict (no schema literal) so it is legal inside a lambda body.
# render_k8s_service (`_render_extra_container`) resolves the image ref, pull
# policy, and the restricted-PSA container securityContext from this.
_project_container = lambda c: Container -> {str: any} {
    {
        name = c.name
        image = c.image
        command = (c.command if c.command else []) + (c.args if c.args else [])
        env_vars = env_project(c.env)
        ports = c.ports if c.ports else []
    }
}

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
#   Service.health      -> K8sWorkload.health_probe    (liveness + readiness)
#   Service.security    -> K8sWorkload.security_override (PSA-default overrides)
#   Service.sidecars    -> K8sWorkload.sidecars      (projected [Container])
#   Service.init        -> K8sWorkload.init_containers (projected [Container])
#
# The k8s escape hatch (`Service.k8s` : K8sOverrides) is applied HERE, by
# the adapter — NOT as a post-render merge. Each typed knob projects onto a
# K8sWorkload field (which render_k8s_service threads into the Deployment /
# RBAC / owned-manifest stream), so the override is a structural by-name
# splice, and a Service with no `k8s` block projects byte-identically:
#   svc.k8s.service_account -> K8sWorkload.service_account
#   svc.k8s.namespaced_rbac -> K8sWorkload.namespaced_rbac
#   svc.k8s.operator        -> K8sWorkload.operator_cluster_rbac + operator_crds
#                              (marks this workload an OPERATOR: the renderer
#                              emits cluster RBAC instead of namespaced RBAC)
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

    _sidecars = [_project_container(c) for c in svc.sidecars] if svc.sidecars else []
    _init = [_project_container(c) for c in svc.init] if svc.init else []

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
        # Health probe — the adapter reads this in render_k8s_service to emit
        # liveness + readiness on the primary container. Unset => no probes
        # (byte-identical to before).
        if svc.health:
            health_probe = svc.health
        # securityContext deltas — the adapter reads these in
        # render_k8s_service to override the restricted-PSA default. Unset =>
        # field absent => byte-identical restricted default.
        if svc.security:
            security_override = svc.security
        # Extra pod containers — projected to the neutral shape
        # render_k8s_service renders (image ref + PSA container SC). Empty =>
        # fields absent => single-container render is byte-identical.
        if _sidecars:
            sidecars = _sidecars
        if _init:
            init_containers = _init
        # k8s escape hatch — applied by the adapter, namespaced under `k8s`.
        if _k8s:
            if _k8s.service_account:
                service_account = _k8s.service_account
            if _k8s.namespaced_rbac:
                namespaced_rbac = _k8s.namespaced_rbac
            # Operator tail — presence of `operator_cluster_rbac` is the
            # renderer's "this is an operator" predicate: _render_cluster_service
            # then emits a ClusterRole/ClusterRoleBinding instead of the
            # namespaced Role. `operator_crds` rides the deploy-as-data JSON as
            # the codegen (AddToScheme/SetupWithManager) bridge.
            if _k8s.operator:
                if _k8s.operator.cluster_rbac:
                    operator_cluster_rbac = _k8s.operator.cluster_rbac
                if _k8s.operator.crds:
                    operator_crds = _k8s.operator.crds
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
# !!! OPERATOR SKIP (OPERATOR_AGNOSTIC.md §5) !!! A Service whose `k8s` block
# sets `operator` (svc.k8s.operator != Undefined) is a Kubernetes OPERATOR — a
# controller-runtime manager that watches the API server. It has NO
# docker-compose meaning (no API server, no Lease, no ClusterRole, no CRD): a
# compose "operator" that starts but reconciles nothing is worse than a clear
# absence. A real compose adapter's SERVICE-LIST builder therefore OMITS such a
# workload from the compose stack ENTIRELY (skip, NOT a hard error — a mixed
# stack still brings up its compose-able half), and `forge audit` surfaces a
# finding for it. The predicate is `svc.k8s.operator != Undefined`. This
# single-service sketch has no list level to skip from, but it never errors and
# never renders the operator tail (svc.k8s is unreferenced below).
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
    # `from_field` (the k8s DOWNWARD API) has NO compose analogue — there is
    # no pod/container to read metadata.name/status.podIP from — so the
    # compose adapter SKIPS those vars entirely (they simply don't appear in
    # `environment`). This is the honest "a target that can't express a
    # channel drops it" case, made explicit here.
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
        # health probe -> compose `healthcheck`. http_path lowers to an HTTP
        # GET via wget; tcp lowers to an `nc -z` port-open check. Timing maps
        # 1:1 (period->interval, timeout, failure_threshold->retries,
        # initial_delay->start_period). Same abstract probe, per-target encoding.
        if svc.health:
            _probe_port = svc.health.port if svc.health.port else (svc.ports[0] if svc.ports else 8080)
            "healthcheck" = {
                if svc.health.http_path != Undefined:
                    "test" = ["CMD", "wget", "--spider", "-q", "http://localhost:" + str(_probe_port) + svc.health.http_path]
                else:
                    "test" = ["CMD-SHELL", "nc -z localhost " + str(_probe_port) + " || exit 1"]
                "interval" = str(svc.health.period_seconds) + "s"
                "timeout" = str(svc.health.timeout_seconds) + "s"
                "retries" = svc.health.failure_threshold
                "start_period" = str(svc.health.initial_delay_seconds) + "s"
            }
        # securityContext -> the compose knobs that have a sensible analogue:
        # run_as_user -> `user` ("uid[:gid]"), privileged -> `privileged`. The
        # rest (fs_group / run_as_non_root / read_only_root_fs) have no compose
        # equivalent and are skipped — this is the honest per-target lowering.
        if svc.security:
            if svc.security.run_as_user != Undefined:
                "user" = str(svc.security.run_as_user) + (":" + str(svc.security.run_as_group) if svc.security.run_as_group != Undefined else "")
            if svc.security.privileged != Undefined:
                "privileged" = svc.security.privileged
        # NOTE: sidecars / init have no in-service compose analogue — a
        # compose sidecar is a SEPARATE top-level compose service, and compose
        # has no init-container concept (use `depends_on` + a one-shot service).
        # This single-service sketch SKIPS them; a real compose adapter would
        # emit sidecars as sibling services keyed by `svc.name + "-" + c.name`.
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
