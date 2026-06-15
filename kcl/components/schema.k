"""
Forge KCL — component model (deploy-as-data normalization).

forge no longer hand-writes `forge.Service` entries as KCL text. It
serializes a DENORMALIZED, k8s-agnostic component shape to
`deploy/kcl/components_gen.json` (name, kind, ports, env, command,
schedule). THIS file owns ALL normalization: the typed `Component`
base schema plus its kind-selected subtypes expand each entry into the
right Kubernetes resources.

The inheritance hierarchy:

    Component (base: name, kind, ports, env, command + per-env overlay)
      ├── Server(Component)   → Deployment + Service (named ports)
      ├── Worker(Component)   → Deployment (no Service)
      ├── Cron(Component)     → CronJob (uses schedule)
      ├── Operator(Component) → manager Deployment + cluster RBAC
      └── Binary(Component)   → Deployment running ["/app/<proj>", "<name>"]

`kind` selects the subtype. forge JSON and these schemas are
deliberately NOT 1:1 — the JSON is the minimal denormalized base, and
KCL inheritance + defaults do the expansion into k8s shapes.

Per-env overlays (replicas, resources, registry, extra env, secrets)
live in `deploy/kcl/<env>/main.k`: that file imports the generated JSON
via `components(option("components"))` (or a literal list), layers the
env knobs on, and passes the result to `forge.render_components(...)`.

KCL v0.11 quirk — schema literals in lambda bodies
==================================================
Same constraint documented in schema.k: lambda bodies can't
instantiate fully-qualified schema literals. The expansion lambdas in
`lib/components.k` therefore type their params as `any` and emit plain
manifest dicts directly rather than constructing `Server{...}` inside a
lambda.
"""

# ---------------------------------------------------------------------------
# Component base + kind subtypes.
#
# A Component carries the denormalized base shape (from
# components_gen.json) PLUS the per-env overlay knobs a project's
# `deploy/kcl/<env>/main.k` sets. The base fields mirror the JSON;
# the overlay fields default so the JSON can be loaded verbatim and
# overlaid via the `|` merge operator.
# ---------------------------------------------------------------------------

schema ComponentPort:
    """One named port from components_gen.json. `expose` flags ports
    that surface on the k8s Service (servers) and Dockerfile EXPOSE.
    """
    name: str
    port: int
    protocol: str = "tcp"
    expose: bool = False

    check:
        name, "ComponentPort.name is required"
        port > 0 and port < 65536, "ComponentPort.port must be 1..65535"

schema Component:
    """Base component shape. The denormalized data forge emits, plus
    the per-env overlay knobs the env's main.k layers on. `kind`
    selects which subtype's expansion runs; project code instantiates
    the subtype directly (Server/Worker/Cron/Operator/Binary) so the
    expansion is type-directed, not a runtime switch.

    Base fields (from components_gen.json):
      name / kind / ports / env / command / schedule / group / version / crds

    Overlay fields (per-env, default so the JSON loads verbatim):
      image / registry / namespace / replicas / resources / env_vars /
      health_check / platform / leader_election
    """
    # --- Denormalized base (forge.yaml -> components_gen.json) ---
    name: str
    kind: str
    ports: [ComponentPort] = []
    # `env` is the base default env from forge.yaml (k8s-agnostic
    # string map). Projected into container env alongside `env_vars`.
    env: {str: str} = {}
    command: [str] = []
    schedule: str = ""
    group: str = ""
    version: str = ""
    crds: [str] = []

    # --- Per-env overlay (set in deploy/kcl/<env>/main.k) ---
    # The container image. Defaults to the component name; env files
    # typically set it to the project image so a shared binary's
    # components all run the same image.
    image: str = ""
    registry: str = ""
    namespace: str = ""
    replicas: int = 1
    # resources / env_vars / health_check mirror the parent forge
    # package's Resources / EnvVar / HealthCheck shapes but are typed
    # loosely here so a per-env main.k can overlay either a forge.*
    # literal or a plain dict without cross-package type-identity
    # friction. The expand layer reads them dynamically.
    resources?: {str:any}
    # Extra env vars (secret/config-map/value backed) layered on top of
    # the base `env` string map. Per-env secrets go here. Each entry is
    # a forge.EnvVar-shaped dict ({name, value?/secret_ref/config_map_ref}).
    env_vars: [{str:any}] = []
    health_check?: {str:any}
    platform: str = ""
    leader_election: bool = True

    check:
        name, "Component.name is required"
        replicas > 0, "Component.replicas must be positive"

schema Server(Component):
    """A Connect-RPC / HTTP server. Expands to a Deployment + Service.
    Named ports become both containerPorts and ServicePorts. The
    primary `http` port (or 8080) drives the readiness/liveness probe.
    """
    kind: str = "server"

    check:
        kind == "server", "Server.kind must be 'server'"

schema Worker(Component):
    """An in-process background worker. Expands to a Deployment only —
    no Service, workers are not addressed by other pods.
    """
    kind: str = "worker"

    check:
        kind == "worker", "Worker.kind must be 'worker'"

schema Cron(Component):
    """A scheduled job. Expands to a CronJob using `schedule` (or a
    one-shot Job when schedule == "").
    """
    kind: str = "cron"

    check:
        kind == "cron", "Cron.kind must be 'cron'"
        schedule == "" or schedule in ["@hourly", "@daily", "@weekly", "@monthly", "@yearly"] or \
            len(schedule.split(" ")) == 5, \
            "Cron.schedule must be '' (one-shot Job), a named expression, or a 5-field cron expression"

schema Operator(Component):
    """A controller-runtime manager. Expands to a manager Deployment +
    cluster-scoped RBAC. `crds` lists the CRD kinds it reconciles.
    """
    kind: str = "operator"

    check:
        kind == "operator", "Operator.kind must be 'operator'"
        len(crds) > 0, "Operator.crds must list at least one CRD kind"

schema Binary(Component):
    """A standalone cobra subcommand off the shared image. Expands to a
    Deployment whose container runs `command` (["/app/<proj>", "<name>"]
    as emitted by forge). No Service — binaries are not Connect surfaces.
    """
    kind: str = "binary"

    check:
        kind == "binary", "Binary.kind must be 'binary'"
        len(command) > 0, "Binary.command must be set (forge emits ['/app/<proj>', '<name>'])"
