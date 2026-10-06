"""
Forge KCL — the ONE workload declaration (ADR 0002), and the runtimes it
binds to.

    fw.Workload  (== forge.Workload)   everything that runs
    forge.OnHost | forge.OnCompose | forge.OnCluster | forge.OnHosted | forge.BuildOnly
                                       where it runs, chosen per workload

A workload is declared ONCE (deploy/kcl/workloads.k) and every env binds it
to a runtime with the same `|` it uses for any other refinement:

    wl.api | {runtime = forge.OnCluster {target = _k3d}}

Every workload in an env states its own runtime; there is no env-level
default, and an unbound workload is a render error naming it. Its runtime-independent
facts (command, args, ports, env, probes, build) are never re-stated per
runtime: the host argv is derived in Go from `build` + `args`, and the
cluster image from the build output.

ONE FIELD VOCABULARY: THE GENERATED ONE
=======================================
The wire contract is pkg/deploy/v1alpha1.WorkloadSpec, and its KCL mirror
`tiers.Workload` is GENERATED (kcl/tiers/tiers_gen.k). This schema uses the
generated field NAMES (camelCase, the JSON the wire carries) and the
generated field TYPES (`tiers.Port`, `tiers.Probes`, `tiers.Resources`,
`tiers.PolicyRule`, `tiers.Container`, ...), so an author writes exactly
the vocabulary a Workload CR carries, and a `fw.Port` IS a `tiers.Port`.

Why it restates the fields instead of inheriting `tiers.Workload`. Three
authoring fields differ from the wire in TYPE, not only in presence, and
KCL refuses to change a field's type in a subschema ("can't change schema
field type of 'env'"):

  * `env` is a MAP `{name: value | reference}`, not the wire's
    `[EnvVar]` list. A map cannot hold two of a name, so the duplicate-env
    footgun (a Deployment kubectl refuses) is unrepresentable, and a
    per-env override is plain `|`. The lowering projects it to the list.
  * `image` is OPTIONAL. The wire requires one, but for a forge-built
    workload the lowering derives it from the build output and the env's
    registry, tag and digests; the author never writes a registry.
  * `probes` may be absent and still end up explicit: the lowering writes
    the default (`_default_probes`) into the spec.

Embedding a `spec: tiers.Workload` instead would make every author write
`spec = {...}` and break the flat `wl.api | {replicas = 3}` refinement that
envs already use. So the fields are stated here with the generated types,
and `TestAuthoringWorkloadCoversSpec` (internal/codegen) fails when the
generated spec gains a field this schema does not carry, so the two
cannot drift.

The lowering (kcl/render.k `_lower`) builds a `tiers.Workload` from this
value and runs the generated `workload_json` on it, so the spec JSON is the
generated projection byte for byte, and every generated field check runs.
"""

import regex
import .tiers
import .lib.annotations as annotations_lib
import .lib.capabilities as caps

# The BROADCAST entry for `Workload.before` (v1alpha1.BeforeAll).
BEFORE_ALL: str = "*"

# ---------------------------------------------------------------------------
# Env references: the values of `Workload.env` that are not a literal.
#
# Each IS the generated wire type of its channel (so its field checks run),
# under an author-facing name. The lowering picks the channel by type.
# ---------------------------------------------------------------------------

schema SecretRef(tiers.SecretKeyRef):
    """One key of a Kubernetes Secret: `secretRef` on the wire.

        env = {DATABASE_URL = forge.SecretRef {name = "db", key = "url"}}

    `optional = True` lets the pod start when the key is absent (and exempts
    it from forge's secret pre-flight). Full profile only: a hosted workload
    reads credentials through `forge.ManagedSecret`.

    `store_key` is set by config codegen, never by hand. It names the value
    in the env's secret STORE that this Secret key is rendered from, so a
    config credential that binds to a HOSTED workload lowers to
    `managedSecret {name = store_key}` instead: the same declaration reads
    the same stored value on every runtime.
    """
    store_key?: str

schema ConfigMapRef(tiers.ConfigMapKeyRef):
    """One key of a ConfigMap in the workload's namespace: `configMapRef`
    on the wire. Full profile only."""

schema FieldRef:
    """A pod field through the Downward API ("metadata.name",
    "status.podIP"): `fieldRef` on the wire. Full profile only."""
    path: str

    check:
        len(path) >= 1 and len(path) <= 253, "FieldRef.path must be 1..253 characters (e.g. 'metadata.name')"

schema ManagedSecret(tiers.ManagedSecretRef):
    """A value in the environment's secret store, by name:
    `managedSecret` on the wire. The runtime materializes it (hosted: the
    control plane; cluster: forge). `optional = True` lets the workload
    start without it."""

schema DatabaseRef(tiers.DatabaseRef):
    """A credential of a `forge.ManagedDatabase` in this env:
    `databaseRef` on the wire. `key` defaults to the connection URI."""

type EnvValue = str | SecretRef | ConfigMapRef | FieldRef | ManagedSecret | DatabaseRef | WorkloadURL

# ---------------------------------------------------------------------------
# Runtimes. The `type` discriminator is what the JSON contract dispatches on
# (output.workloads[].runtime.type), so it is stated on each member.
# ---------------------------------------------------------------------------

schema OnHost:
    """A local process `forge env up` launches (ADR 0002 §2).

    The argv is DERIVED in Go (hostlaunch.BuildCmd) from the runner, the
    workload's `build` and its `args` — there is no command here:

      * go-run — `go run <build.cmd> <args>`
      * binary — `./bin/<output_name> <args>`
      * delve  — `dlv exec --headless … ./bin/<output_name> -- <args>`
      * air    — `air -c <air_config>`; the air config owns the argv.

    A workload with an explicit `command` runs `command + args` verbatim
    (a sibling repo's binary, a third-party tool). Env comes from the
    workload's `env`, the same as on every runtime.

    `listen_ports` are the TCP ports the process itself BINDS: the
    port-conflict pre-flight and the readiness gate check exactly these,
    and a `forge.WorkloadURL` to this workload resolves to the first one.
    An explicitly EMPTY list means "binds nothing" (a desktop app) and is
    different from unset (fall back to the workload's `ports`).
    """
    type: "host" = "host"
    runner: str = "go-run"
    air_config?: str
    # Working directory for the process. Relative paths resolve against the
    # project root. For a cross-repo binary whose runner config resolves
    # relative to a sibling repo. Default: the project root.
    working_dir?: str
    listen_ports?: [int]
    delve_port: int = 2345

    check:
        runner in ["go-run", "air", "binary", "delve"], \
            "OnHost.runner must be 'go-run' | 'air' | 'binary' | 'delve' — got '${runner}'"
        air_config == Undefined or runner == "air", \
            "OnHost.air_config is only valid when runner = 'air'"
        delve_port > 0 and delve_port < 65536, "OnHost.delve_port must be 1..65535"
        listen_ports == Undefined or all p in listen_ports { p > 0 and p < 65536 }, \
            "OnHost.listen_ports entries must be 1..65535"

schema OnCompose:
    """A docker-compose service. Compose owns the container definition
    (image, healthcheck, ports, scale); the workload contributes its name,
    this block, and its literal env, which is merged into `env` below.

    `env` is the compose PROCESS environment — what the compose file's own
    `${VAR}` references interpolate from (an `env_file` only reaches
    containers). It is what lets a port be declared once in KCL and reach
    the file. KCL values WIN over the inherited shell: a shell override
    would move the container while every other reference stayed put.

    `wait` (default) gates on the service's compose healthcheck, so a
    workload that depends on it starts against something serving.
    `wait_timeout` is a ceiling in seconds, 0 = none. Requires docker
    compose v2.17.0+.

    `shared` declares that this compose stack is MACHINE infrastructure
    every git worktree of the repo uses — one postgres, one NATS — rather
    than something each checkout runs for itself. forge then drives it from
    the repo's PRIMARY checkout, whichever worktree runs `forge env up`:
    `file` and `env_file` resolve there, and so does every relative bind
    mount in the file. Without it, a linked worktree's compose project has
    the same NAME (the directory basename) but different absolute mount
    sources, so compose sees a changed config and RECREATES the shared
    containers out from under every other stack, then leaves them mounting
    files inside that worktree. Pair it with `fp.write_file(...,
    shared=True)` for any config the shared stack mounts.
    """
    type: "compose" = "compose"
    # The compose service name. Default: the workload's name.
    service?: str
    file: str = "docker-compose.yml"
    env_file?: str
    wait: bool = True
    wait_timeout: int = 0
    env: {str: str} = {}
    shared: bool = False

    check:
        wait_timeout >= 0, "OnCompose.wait_timeout must be >= 0 (0 = no explicit ceiling)"

schema OnCluster:
    """A Kubernetes cluster you operate. The workload renders as a
    `forge.dev/v1alpha1 Workload` record in `output.manifests`, and forge
    expands it through pkg/deploy.RenderWorkloads under the Full profile.
    `target` names the kubectl context, namespace and registry."""
    type: "cluster" = "cluster"
    target: ClusterTarget

schema OnHosted:
    """The forge control plane (Reliant cloud or another): the workload's
    spec is published as a Workload CR and the platform renders it under
    the Restricted profile. Requires `Bundle.control_plane`. The platform
    owns every placement fact except one: the CPU architecture its
    nodes run, which forge needs at BUILD time. `platform` declares it (the
    reliant platform's node pools are amd64), so a hosted image's arch is
    a property of the platform, never of the machine running `forge build`
    (an arm64 Mac would otherwise push an arm64 image the nodes cannot exec).
    Override only for a control plane whose pools are arm64."""
    type: "hosted" = "hosted"
    platform: str = "amd64"

    check:
        platform in ["amd64", "arm64"], "OnHosted.platform must be 'amd64' or 'arm64' (the GOARCH of the control plane's platform's nodes)"

schema BuildOnly:
    """Built and shipped, never run: a CLI in a release artifact, an image
    other workloads pull. `build_variants` emits several binaries from one
    Go build (different ldflags / tags / arch)."""
    type: "build-only" = "build-only"
    build_variants: [BuildVariant] = []

type Runtime = OnHost | OnCompose | OnCluster | OnHosted | BuildOnly

_invalid_annotation_keys = lambda a: any -> [str] {
    annotations_lib.invalid_keys(a) if a else []
}

# ---------------------------------------------------------------------------
# An app-flow health check.
# ---------------------------------------------------------------------------

schema FlowCheck:
    """An end-to-end invariant only the app can assert, exposed by the
    workload that holds the state as an HTTP endpoint: 2xx is healthy,
    anything else (typically 503) is not.

    `forge env smoke <env>` curls it. A route probe proves listeners are up;
    a flow check proves the APP works — smoke can be green while the
    managed-daemon flow is broken, because no built-in probe can express
    "every Ready daemon is attached to the gateway". The owning workload
    asserts that internally and answers 200/503, so smoke needs only a URL —
    no database credentials, no privileged command.

    DECLARED ON THE WORKLOAD that serves the endpoint, so it exists exactly
    where the workload does: an env that does not run the workload does not
    run its check, and there is no env filter to keep in step.

    WHERE THE URL COMES FROM — never written down, so there is no port to go
    stale. forge resolves it in the env being smoked, in this order:

      1. an HTTPRoute/GRPCRoute to the workload whose `path` covers this
         check's `path` (the route forwards it): the listener's origin
         plus `path`;
      2. a host workload's first listen port: http://localhost:<port><path>;
      3. a hosted workload's platform URL, from the control plane's status.

    A workload none of these reaches (a cluster workload with no route
    covering `path`) is reported by smoke as misdeclared, naming the fix —
    never silently skipped.
    """
    # The endpoint path on the workload, e.g. "/flow-health". Starts with "/".
    path: str
    # What the check asserts, in a sentence. Shown beside the verdict.
    description?: str
    # Label in the smoke table and --json. Default: "<workload>:<path>".
    name?: str

    check:
        path.startswith("/"), "FlowCheck.path must start with '/' (e.g. '/flow-health') — got '${path}'"

# ---------------------------------------------------------------------------
# The workload.
# ---------------------------------------------------------------------------

schema Workload:
    """Everything that runs: a service, worker, job, cron, operator or tool.

    The fields below `runtime` are v1alpha1.WorkloadSpec's, by the same
    names and types (see the module docstring). Every field has a default
    that is valid on its own, so a base declaration is COMPLETE and an env
    states only what it changes.
    """
    # --- Authoring-only (consumed by the lowering; not on the wire) ---
    name: str
    # HOW this workload's artifact is produced. Unset: forge builds
    # nothing for it (a third-party image, a compose service, a sibling
    # repo's binary). There is no synthesized default.
    build?: GoBuild | DockerBuild | ShellBuild | RemoteBuild
    # WHERE it runs. Required by the time the env renders it: the env binds
    # it (`wl.x | {runtime = ...}`), since the base declaration in
    # workloads.k is runtime-independent.
    runtime?: Runtime
    # The SENSITIVE config fields this workload reads, by ENV_VAR name.
    # Pass it to the generated `config_gen.<x>ConfigEnvMap(cfg,
    # config_secrets)`, which projects a secret for exactly these names:
    # a credential reaches only the workloads that declare it, so one
    # feature's missing Secret key cannot stall every pod in the namespace
    # (CreateContainerConfigError, no application log). Non-sensitive
    # config needs no entry: it projects to every workload as a value.
    config_secrets: [str] = []
    # App-flow health checks `forge env smoke` runs against this workload.
    # See FlowCheck.
    flow_checks: [FlowCheck] = []
    # The components this workload's process runs besides the one it is
    # named for, by component name. A subcommand that mounts several
    # components (control-plane's `admin-api` serves six admin services) is
    # ONE workload, and which components it mounts is decided in the
    # project's Go, where forge cannot see it; this states it. `forge lint`
    # then does not report those components as undeployed, and `forge
    # scaffold` does not declare a second workload for one. Nothing renders
    # from it: it never changes what deploys.
    serves?: [str]

    # --- v1alpha1.WorkloadSpec ---
    kind: str = "service"
    # A third-party image, or the artifact name of a forge-built one.
    # Unset on a built workload: the build's output_name, else the name.
    image?: str
    command?: [str]
    # The entrypoint's arguments — for a forge-built binary, the
    # subcommand. The same args select it on every runtime.
    args?: [str]
    replicas: int = 1
    resources?: tiers.Resources
    # name -> a literal, or a reference: forge.SecretRef | ConfigMapRef |
    # FieldRef | ManagedSecret | DatabaseRef | WorkloadURL. Insertion order
    # is kept (Kubernetes `$(VAR)` expansion reads only EARLIER entries).
    env: {str: EnvValue} = {}
    ports?: [tiers.Port]
    probes?: tiers.Probes
    storageGiB?: int
    schedule?: str
    before?: [str]
    deployPhase?: str
    namespacedRBAC?: [tiers.PolicyRule]
    clusterRBAC?: [tiers.PolicyRule]
    # The custom resource kinds an operator's manager owns; forge derives its
    # ClusterRole rules on them (with `group`). May be empty — see the check.
    crds?: [str]
    group?: str
    version?: str
    leaderElection?: bool
    serviceAccount?: str
    serviceAccountAnnotations?: {str:str}
    sidecars?: [tiers.Container]
    volumes?: [tiers.Volume]
    nodeSelector?: {str:str}
    tolerations?: [tiers.Toleration]
    podAnnotations?: {str:str}
    # How the Deployment replaces its pods: "RollingUpdate" (the Kubernetes
    # default — surges a new pod before the old one stops, so two versions
    # run at once) or "Recreate" (stops every old pod first, so exactly one
    # version ever runs, at the cost of a gap with none). Declare Recreate
    # when the replica count is a CORRECTNESS bound rather than a capacity
    # choice. Refused beside storageGiB when RollingUpdate: a ReadWriteOnce
    # volume deadlocks the surge pod.
    strategy?: str
    # Ranks these pods against everything else competing for a node, so a
    # pod is not preempted by neighbours that carry a class. The
    # PriorityClass is cluster-scoped and must ALREADY EXIST on every
    # cluster this lands on — a pod naming a missing one is refused at
    # admission. Unset = the cluster default. Full profile only.
    priorityClassName?: str
    # Pod-identity overrides (uid/gid, fsGroup, read-only root filesystem).
    securityContext?: tiers.PodSecurity
    # Overrides the grace period Go derives from the drain env
    # (PRE_STOP_DELAY + SHUTDOWN_TIMEOUT). Full profile only.
    terminationGracePeriodSeconds?: int
    # A job or cron run's deadline. On the host runtime it is the process
    # timeout.
    activeDeadlineSeconds?: int

    check:
        regex.match(name, r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$") and len(name) <= 63, \
            "Workload.name '${name}' must be an RFC-1123 label of at most 63 characters (it becomes every object's name)"
        kind in caps.WORKLOAD_KINDS, \
            "Workload.kind must be one of ${caps.WORKLOAD_KINDS} — got '${kind}'. A kind says HOW this runs, not what code is inside it."
        replicas > 0, "Workload '${name}': replicas must be positive"

        # --- cron ---
        not schedule or kind == "cron", \
            "Workload.schedule is only meaningful for kind='cron' — a '${kind}' has no schedule"
        kind != "cron" or schedule, "Workload '${name}': kind='cron' requires a schedule"
        not schedule or schedule in ["@hourly", "@daily", "@weekly", "@monthly", "@yearly"] or len(schedule.split(" ")) == 5, \
            "Workload.schedule must be a named expression (@hourly, @daily, ...) or a 5-field cron expression — got '${schedule}'"

        # --- job ---
        kind != "job" or command or args, \
            "Workload '${name}': kind='job' requires a command or args — a one-shot with nothing to run is not a job"
        not before or kind == "job", \
            "Workload.before orders a one-shot job; it is only valid for kind='job'"
        not before or name not in before, "Workload.before must not name the workload itself"
        not before or BEFORE_ALL not in before or len(before) == 1, \
            "Workload.before = ['${BEFORE_ALL}'] already gates every workload — remove the other names (got ${before}), or drop the wildcard and enumerate."
        not deployPhase or deployPhase in ["pre-rollout", "post-rollout"], \
            "Workload.deployPhase must be 'pre-rollout' or 'post-rollout' — got '${deployPhase}'"
        not deployPhase or (kind == "job" and not before), \
            "Workload.deployPhase places a standalone one-shot Job (kind='job', no `before`) in the deploy; '${name}' is a '${kind}'${' with `before`' if before else ''} — remove deployPhase"

        # --- operator ---
        # No minimum on `crds`. An operator with none is real: one that owns
        # no CRD yet (`forge scaffold operator` declares it before `forge
        # scaffold crd` adds the first), or one that reconciles built-in
        # kinds only and states what it needs in `clusterRBAC`. `crds` is what
        # its derived ClusterRole covers, and an empty list derives nothing.

        # --- kind x capability (lib/capabilities.k) ---
        # A declaration the kind cannot express is REJECTED, naming the
        # field, the kind and the alternative, rather than rendered and
        # silently dropped.
        not namespacedRBAC or caps.supports(kind, caps.CAP_NAMESPACED_RBAC), \
            "Workload.namespacedRBAC is not supported for kind='${kind}' — a tool is never scheduled, so it has no pod to grant permission to (use kind='service' or kind='worker')."
        # A GATING job runs as an initContainer under the DEPENDENT's
        # ServiceAccount, so a Role for it would bind nothing.
        not namespacedRBAC or not before, \
            "Workload.namespacedRBAC is not supported on a gating job (before = ${before}): it runs as an initContainer in each dependent's pod, under the DEPENDENT's ServiceAccount, so a Role for '${name}' would bind nothing. Grant the rules on the dependents, or drop `before` to run '${name}' as a standalone Job with its own identity."
        not clusterRBAC or caps.supports(kind, caps.CAP_CLUSTER_RBAC), \
            "Workload.clusterRBAC is not supported for kind='${kind}' — a tool is never scheduled, so it has no pod to grant permission to."
        not activeDeadlineSeconds or kind in ["job", "cron"], \
            "Workload.activeDeadlineSeconds bounds a run to completion — only kind='job' or 'cron' completes; '${name}' is a '${kind}'"
        not ports or caps.supports(kind, caps.CAP_PORTS), \
            "Workload.ports is not supported for kind='${kind}' — a tool is never scheduled and a batch kind (cron/job) runs to completion, so there is no pod anything can dial. Use kind='service'."
        replicas == 1 or caps.supports(kind, caps.CAP_REPLICAS), \
            "Workload.replicas is not supported for kind='${kind}' — a tool is never scheduled, and a batch pod's concurrency belongs to the batch object, so Kubernetes has nowhere to put this value."
        not probes or caps.supports(kind, caps.CAP_PROBES), \
            "Workload.probes is not supported for kind='${kind}' — a batch pod runs to completion, so a liveness probe would restart work that is merely still running, and a tool is never scheduled."
        not serviceAccountAnnotations or caps.supports(kind, caps.CAP_WORKLOAD), \
            "Workload.serviceAccountAnnotations is not supported for kind='${kind}' — a tool is never scheduled, so forge renders no ServiceAccount to annotate."
        len(_invalid_annotation_keys(serviceAccountAnnotations)) == 0, \
            "Workload '${name}': serviceAccountAnnotations has invalid annotation key(s) ${_invalid_annotation_keys(serviceAccountAnnotations)}: a key is `[prefix/]name`, where name is <= 63 chars of [A-Za-z0-9-_.] starting and ending alphanumeric, and prefix is a DNS subdomain (e.g. iam.gke.io/gcp-service-account)"
        not (serviceAccount and (serviceAccountAnnotations or namespacedRBAC or clusterRBAC)), \
            "Workload '${name}': serviceAccount = '${serviceAccount}' runs the pod as an EXISTING identity, so forge generates none — serviceAccountAnnotations and RBAC would land nowhere. Put them on '${serviceAccount}' where it is provisioned."
        not crds or caps.supports(kind, caps.CAP_CRDS), \
            "Workload.crds is only supported for kind='operator' — got '${kind}'. `crds` names the custom resources a controller-runtime manager owns."
        not storageGiB or storageGiB == 0 or replicas == 1, \
            "Workload '${name}': storageGiB needs replicas = 1 — one disk is attached to one pod (a Recreate rollout)"
