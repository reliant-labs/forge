"""
Forge KCL — the workload model (deploy-as-data normalization).

A WORKLOAD is one deployable unit: an executable, the args that select
what it does, and where/how it runs. In Kubernetes terms it is what
becomes one Deployment (or CronJob, or Job). That is the whole
definition, and it is deliberately narrow.

A workload is NOT a proto service. One binary can mount a dozen
Connect services on a shared mux and still be ONE workload, because it
is one process with one image and one set of args. Declaring a workload
per proto service is the mistake this vocabulary exists to prevent:
it renders N Deployments for what is really one process.

  proto service  = a set of RPCs, mounted on a mux (code)
  workload       = an image + args + placement (deploy)

`deploy/kcl/workloads.k` declares what a project is made of, one
`Workload` per deployable unit. That file is scaffolded once and owned
by the project from then on — forge never rewrites it. THIS file owns
ALL normalization: the `Workload` schema plus the kind-selected
expansion that turns each entry into the right Kubernetes resources.

WHY ONE SCHEMA WITH A `kind`, NOT SIX SUBSCHEMAS
================================================
The kinds share every field and differ ONLY in which expansion runs.
That is a DISCRIMINATOR, not a type hierarchy: subschemas that add no
fields buy nothing but a longer name to remember, and they make the
one thing users actually do — refine a base declaration per env —
require knowing which subtype to name. With a single schema,
`prod_api = base.api | {replicas = 3}` type-checks against the SAME
schema in every environment, so its `check:` block re-runs on the
merged result and validates the override.

  kind = "service"  → Deployment + Service (named ports)
  kind = "worker"   → Deployment (no Service; nothing dials it)
  kind = "cron"     → CronJob on `schedule`
  kind = "job"      → one-shot: init container on each `before`
                      dependent, else a bare Job
  kind = "operator" → manager Deployment + cluster-scoped RBAC
  kind = "tool"     → built and shipped in the image, never scheduled

WHAT HAPPENED TO `binary`
=========================
It was deleted, because it named the wrong axis. EVERY workload here
is a binary with args — that is the definition of a workload — so
`binary` described a property all six kinds share rather than one that
distinguishes any of them. Its expansion was byte-identical to
`worker`'s.

What it MEANT, in the one place it earned its keep, is "forge builds
this and bakes it into the image, but nothing schedules it" — a CLI, a
one-off admin tool, an escape hatch you `kubectl exec` to run. That is
a real and distinct answer to "how does this run", so it kept a kind
and got a name that says it: `tool`. A `tool` renders NO workload
manifest; it appears in the build contract only.

Note the difference from `job`: a job RUNS, once, on a schedule of
"now", and other workloads wait on it. A tool never runs on its own at
all.

DECLARE COMPLETE, REFINE PER ENV
================================
A `Workload` literal in `deploy/kcl/workloads.k` is a COMPLETE, valid
workload — ports, replicas and all. It is not a fragment awaiting
assembly. Each environment then REFINES it with KCL's `|` merge:

    # deploy/kcl/workloads.k
    api = fw.Workload {
        name = "api"
        kind = "service"
        ports = [fw.Port {name = "http", port = 8080, expose = True}]
    }

    # deploy/kcl/prod/main.k
    _api = wl.api | {replicas = 3, resources = forge.Resources {...}}

This is the inverse of the old model, where the shared file was
deliberately INCOMPLETE (no ports, no replicas) and every env had to
re-apply the same overlay to make it valid. That forced three
near-identical copies of one comprehension and meant no single file
ever showed a whole workload. Now the base file answers "what is
this?" completely, and each env states only what it CHANGES.

Because `|` preserves the schema type, every `check:` below re-runs on
the merged result — so an env that overrides `replicas = 0` is
rejected at render time, in that env, naming the field.

AGGREGATION IS EXPLICIT
=======================
Each env's main.k names the workloads it deploys in a plain list. It is
not collected implicitly, and that is not a limitation being worked
around — `Schema.instances()` does not cross package boundaries in KCL
0.11, and an explicit list is what you want anyway: dev can declare a
containerized postgres that prod omits, and reading the list tells you
what the environment IS.

KCL v0.11 quirk — schema literals in lambda bodies
==================================================
Same constraint documented in schema.k: lambda bodies can't
instantiate fully-qualified schema literals. The expansion lambdas in
`expand.k` therefore type their params as `any` and emit plain
manifest dicts directly rather than constructing `Workload{...}`
inside a lambda.
"""

import .expand as expand
import .capabilities as caps

# ---------------------------------------------------------------------------
# The workload kinds. A kind answers HOW this thing runs — never what
# language it is in, what code is inside it, or how it was built.
#
# WHAT EACH KIND CAN EXPRESS is declared ONCE, as data, in capabilities.k.
# These lists are derived from that matrix rather than restated, so the
# question "does a worker get RBAC?" has exactly one answer in exactly one
# file — and the renderers (expand.k) read the SAME table, which is what
# makes a capability impossible to wire into the schema and forget in the
# expansion, or the reverse.
# ---------------------------------------------------------------------------

# `WORKLOAD_KINDS` (every valid kind — `tool` included, though it renders
# no manifest) and `DEPLOYMENT_KINDS` (the kinds that render a long-running
# Deployment) are DEFINED in capabilities.k, derived from the matrix, and
# referenced qualified from here.
#
# They are not re-declared in this file for the same reason `BEFORE_ALL` is
# not: KCL flattens top-level names across a package for export, so a
# project still writes the unqualified `fw.WORKLOAD_KINDS` — but a second
# declaration here would COLLIDE with that one rather than alias it
# ("Can not change the value of 'WORKLOAD_KINDS', because it was declared
# immutable").

# The BROADCAST selector for `Workload.before` is `expand.BEFORE_ALL`
# ("*") — "gate every workload in this environment", as opposed to an
# enumerated list of dependent names.
#
# It is DEFINED in expand.k, next to the `job_gates` predicate that
# interprets it, and referenced qualified from here. KCL flattens
# top-level names across a package for export — so a project writes the
# unqualified `fw.BEFORE_ALL` — but a second declaration in this file
# would collide with that one rather than alias it.

schema Port:
    """One named port a workload listens on. `expose` flags ports that
    surface on the k8s Service (kind="service") and Dockerfile EXPOSE.

    Declared on the workload itself, because a port is part of what the
    workload IS — the base declaration is complete. An environment that
    genuinely differs overrides it (`wl.api | {ports = [...]}`); the
    common case, where every env serves the same port, states it once.

    A service that declares no ports gets the standard `http` :8080 mux
    its expansion falls back to.
    """
    name: str
    port: int
    protocol: str = "tcp"
    expose: bool = False

    check:
        name, "Port.name is required"
        port > 0 and port < 65536, "Port.port must be 1..65535"

schema Workload:
    """ONE deployable unit: an executable, its args, and its placement.

    What becomes one k8s Deployment / CronJob / Job. NOT a proto
    service — a binary serving twelve Connect services is one Workload.

    `kind` selects the expansion (see WORKLOAD_KINDS). Everything else
    is a fact about how this thing runs, and every field has a default
    that is valid on its own, so a declaration states only what is true
    of it and an env refines the rest with `|`.
    """
    # --- Identity + dispatch ---
    name: str
    # HOW this runs. See WORKLOAD_KINDS.
    kind: str = "service"

    # --- What it executes ---
    # The argv. Empty means the image's own entrypoint, which is the
    # normal case for a project's primary binary. A workload that
    # selects a subcommand states it here (["/app/myapp", "reindex"]).
    command: [str] = []
    # Polymorphic build declaration: a plain dict carrying a `type`
    # discriminator ("go" | "docker" | "shell") plus that type's fields.
    # Typed loosely as `{str:any}` so either a forge.GoBuild literal or a
    # plain dict works across the package boundary without type-identity
    # friction — same reason `resources` / `env_vars` are `any`. Empty
    # means "no build declared": the bridge omits it and the Go side
    # synthesizes the ./cmd/<name> default.
    build: {str:any} = {}

    # --- Scheduling (kind-specific) ---
    # kind="cron": the schedule. Empty renders a one-shot Job.
    schedule: str = ""
    # kind="job": the workloads that must not start until this job exits
    # 0. Empty means "run once, nothing waits" — a standalone Job.
    #
    # `before = [BEFORE_ALL]` (i.e. ["*"]) is the BROADCAST form: gate
    # EVERY workload in the environment, without naming any of them.
    #
    # It exists because the enumerated form cannot express a whole class
    # of one-shot honestly. Schema migration is the motivating case: it
    # must precede every workload in every environment, forever, and an
    # enumerated list of dependents is a list that goes STALE. Add a
    # workload six months from now and an enumerated `before` still
    # renders, still passes every check, and silently does not gate the
    # new one — the failure surfaces as that workload serving traffic
    # against a schema it does not have. A wildcard has nothing to
    # forget to update.
    #
    # It gates deployment-shaped workloads (service / worker / operator)
    # and other, non-broadcast jobs. See BROADCAST_GATED_KINDS in
    # expand.k for what it deliberately does NOT gate, and why.
    before: [str] = []

    # --- Operator-specific ---
    group: str = ""
    version: str = ""
    crds: [str] = []
    leader_election: bool = True

    # --- Placement + runtime ---
    ports: [Port] = []
    # The container image. Defaults to the workload name; projects that
    # ship one image with several entrypoints set it to the project image
    # so every workload runs the same artifact.
    image: str = ""
    registry: str = ""
    namespace: str = ""
    replicas: int = 1
    platform: str = ""
    # Base env as a k8s-agnostic string map, projected into container env
    # alongside `env_vars`.
    env: {str: str} = {}

    # The SENSITIVE config fields THIS workload reads, by ENV_VAR name
    # ("DATABASE_URL", "STRIPE_SECRET_KEY"). Each env's main.k passes this
    # to the generated `config_gen.<x>ConfigEnvMap(cfg, config_secrets)`, which
    # projects a secretKeyRef for exactly these names and no others.
    #
    # NON-sensitive config is NOT listed here and never needs to be. Every
    # field that is not marked `sensitive: true` in proto/config/v1/config.proto
    # projects to an inline value on every workload automatically, so adding
    # an ordinary config value stays a one-line proto edit. This list is only
    # the credentials, which is the short list.
    #
    # WHY A WORKLOAD MUST DECLARE THEM. The config projection used to emit
    # every sensitive field into every workload's env, because one map was
    # layered under all of them. Adding a single credential to a shared
    # AppConfig therefore added a secretKeyRef to every Deployment, CronJob
    # and Job in the project — in control-plane, one new `zitadel_broker_token`
    # landed in 7 Deployments plus a Job that never read it. A pod whose
    # secretKeyRef names a key that does not exist NEVER STARTS: it stalls in
    # CreateContainerConfigError, produces no application log, and gives no
    # hint that the cause is a credential belonging to a different feature. So
    # the first deploy of one feature's secret could take down the entire
    # namespace instead of that feature. Declaring the reader makes the blast
    # radius the workload that actually needs the value.
    #
    # It is also plain least privilege: a compromised pod cannot read a
    # Secret key that is absent from its own manifest.
    #
    # Empty is the correct and common answer — most workloads read no
    # credentials at all. Naming a var that is not a sensitive config field
    # fails the render and names it, rather than being silently ignored.
    config_secrets: [str] = []

    # resources / env_vars / health_check mirror the parent forge package's
    # Resources / EnvVar / HealthCheck shapes.
    #
    # ALL THREE ARE TYPED `any`, and that is load-bearing. A per-env main.k
    # overlays them with real forge.* SCHEMA INSTANCES — `forge.Resources
    # {...}` for resources, `forge.env_merge(...)` (which returns
    # forge.EnvVar values) for env_vars. KCL will not accept a schema
    # instance where `{str:any}` is declared: it fails with "expect
    # {str:any}, got forge.Resources". The loose annotation is what lets an
    # env pass either a typed literal or a plain dict, across the package
    # boundary, without cross-package type-identity friction. The expand
    # layer reads all of them dynamically, so nothing is lost.
    resources?: any
    # Extra env vars (secret/config-map/value backed) layered on top of the
    # base `env` string map. Per-env secrets go here. Each entry is
    # forge.EnvVar-SHAPED ({name, value?/secret_ref/config_map_ref}).
    env_vars: [any] = []
    health_check?: any

    # --- RBAC ---
    #
    # Two fields, two TIERS, and a workload uses at most one of them. Which
    # one a kind may use is the capability matrix's call (capabilities.k):
    # `service` / `worker` / `cron` / `job` take namespaced rules,
    # `operator` takes cluster rules, `tool` takes neither because it is
    # never scheduled. Declaring the wrong one for your kind FAILS the
    # render and names both — see the check block below.
    #
    # Both take the same PolicyRule dict shape:
    #   { apiGroups: [...], resources: [...], verbs: [...] }
    # and both are ADDITIVE to the default config-read rules forge already
    # grants (lib/rbac.k). Unset => no Role at all for the namespaced tier
    # (a bare ServiceAccount: identity, no permission, no mounted token),
    # which is the right default because most workloads never call the
    # Kubernetes API — their config arrives as container env, projected at
    # render time.

    # Namespaced rules — a Role + RoleBinding in the workload's OWN
    # namespace. What a reconcile loop, a sweeper or a convergence step
    # needs: in-namespace access this specific workload does, and nothing
    # else in the namespace should inherit.
    #
    # Until the capability matrix existed only `job` could express this,
    # not because the other kinds did not need it but because nobody had
    # threaded it through their expansions. A `worker` that reaps stale
    # custom resources shipped with a ServiceAccount that could not list
    # them, and the manifests were valid, the Deployment rolled, and the
    # first sweep failed in production with `cannot list resource
    # "workspaces" in API group "reliant.dev"`.
    namespaced_rbac: [{str: any}] = []

    # Cluster-scoped rules — a ClusterRole + ClusterRoleBinding. Only
    # `operator` may set these, because watching resources ACROSS
    # namespaces is what distinguishes an operator from a service that
    # happens to read the API.
    #
    # The tier is a REPLACEMENT, not an addition: an operator gets a
    # ClusterRole INSTEAD OF a Role. Emitting both would put two bindings
    # on one ServiceAccount, where the wider one decides every outcome —
    # so the narrower Role would describe permissions that are not the
    # permissions in force, and deleting it would change nothing.
    #
    # A `service` that needs cluster scope should be declared
    # `kind = "operator"`. That is not a workaround: it is what the kind
    # means, and it also gets you leader election and the CRD wiring, which
    # a workload watching cluster-wide state almost certainly wants too.
    cluster_rbac: [{str: any}] = []

    check:
        name, "Workload.name is required"
        kind in caps.WORKLOAD_KINDS, \
            "Workload.kind must be one of ${caps.WORKLOAD_KINDS} — got '${kind}'. A kind says HOW this runs, not what code is inside it."
        replicas > 0, "Workload.replicas must be positive"

        # --- cron ---
        schedule == "" or kind == "cron", \
            "Workload.schedule is only meaningful for kind='cron' — a '${kind}' has no schedule"
        kind != "cron" or schedule == "" or \
            schedule in ["@hourly", "@daily", "@weekly", "@monthly", "@yearly"] or \
            len(schedule.split(" ")) == 5, \
            "Workload.schedule must be '' (one-shot Job), a named expression, or a 5-field cron expression"

        # --- job ---
        # A job with no command has nothing to run to completion; a job is
        # ONE-SHOT, so a schedule is a contradiction (that is a cron).
        kind != "job" or len(command) > 0, \
            "kind='job' requires a command — a one-shot with nothing to run is not a job"
        len(before) == 0 or kind == "job", \
            "Workload.before orders a one-shot job; it is only valid for kind='job'"
        name not in before, "Workload.before must not name the workload itself"
        # The wildcard already means "every workload", so mixing it with
        # names is a contradiction: the names are either redundant or the
        # author believed the list NARROWED the broadcast. Both readings
        # are wrong, and one of them is dangerous, so reject rather than
        # pick a winner.
        expand.BEFORE_ALL not in before or len(before) == 1, \
            "Workload.before = ['${expand.BEFORE_ALL}'] already gates every workload — remove the other names (got ${before}), or drop the wildcard and enumerate."

        # --- operator ---
        kind != "operator" or len(crds) > 0, \
            "kind='operator' must list at least one CRD kind in `crds`"

        # --- CAPABILITY CHECKS (capabilities.k) ---
        #
        # THE POINT OF THESE. A workload that declares something its kind
        # cannot express used to render successfully and silently drop the
        # declaration — which is the failure mode the capability matrix
        # exists to end. A dropped `namespaced_rbac` is a pod that runs
        # without the permission its author wrote down; a dropped
        # `replicas = 3` is one replica serving traffic sized for three.
        # Both look fine in the manifest, because the manifest is the thing
        # that lost the information.
        #
        # So the rule is: if the matrix says this kind cannot express the
        # capability, REJECT the declaration and say which kind can. Each
        # message names the field, the kind, and the alternative — the
        # author knows what they wanted, and the error should tell them
        # where to put it rather than only that it is not allowed here.
        #
        # Every check is written "no declaration OR the kind supports it",
        # so a workload that declares nothing is unaffected — these can only
        # fire on an explicit statement of intent.

        len(namespaced_rbac) == 0 or caps.supports(kind, caps.CAP_NAMESPACED_RBAC), \
            "Workload.namespaced_rbac is not supported for kind='${kind}'. An operator's permissions are CLUSTER-scoped, so an operator uses `cluster_rbac` instead; a tool is never scheduled, so it has no pod to grant permission to (use kind='service' or kind='worker')."

        len(cluster_rbac) == 0 or caps.supports(kind, caps.CAP_CLUSTER_RBAC), \
            "Workload.cluster_rbac is only supported for kind='operator' — got '${kind}'. Watching resources ACROSS namespaces is what makes a workload an operator. For access within this workload's own namespace use `namespaced_rbac`; if it genuinely watches cluster-wide, declare kind='operator' (which also gets you leader election and CRD wiring)."

        len(ports) == 0 or caps.supports(kind, caps.CAP_PORTS), \
            "Workload.ports is not supported for kind='${kind}' — a tool is never scheduled and a batch kind (cron/job) runs to completion, so in neither case is there a pod anything can dial. Use kind='service'."

        replicas == 1 or caps.supports(kind, caps.CAP_REPLICAS), \
            "Workload.replicas is not supported for kind='${kind}' — a tool is never scheduled, and a batch pod's concurrency is a property of the batch object (a CronJob's concurrencyPolicy, a Job's parallelism) rather than a replica count, so k8s has nowhere to put this value."

        len(crds) == 0 or caps.supports(kind, caps.CAP_CRDS), \
            "Workload.crds is only supported for kind='operator' — got '${kind}'. `crds` names the custom resources a controller-runtime manager owns."
