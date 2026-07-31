"""
Forge KCL — component render entry points.

Sibling of schema.k / components.k so it sees Component, Server,
Worker, Cron, Operator, Binary, ComponentPort, Resources, EnvVar as
bare names (no import that would fork type identity).

Two responsibilities:

  1. `load_components(path)` — read the denormalized
     `components_gen.json` forge emits and map each entry to the right
     typed Component subtype (defaults applied). The result is a list
     of typed components a per-env main.k overlays (replicas,
     resources, registry, secrets) via the `|` merge operator.

  2. `render_components(components, env)` — normalize the typed
     components into the Kubernetes manifest list for an environment.
     Each component's kind selects its expansion (Server→Deployment+
     Service, Cron→CronJob, …). Namespace-scoped boilerplate (the
     Namespace object, network policies) is added once.

Path contract: `file.read` resolves relative to the process working
directory. forge runs `kcl run deploy/kcl/<env>` from the project root,
and the documented manual command (`kcl run deploy/kcl/dev`) is also
run from the project root — so the default path
`deploy/kcl/components_gen.json` is correct in both. Pass an explicit
path to `load_components` to override.
"""

import file
import json
import .expand as comp_lib
import ..lib.netpol as netpol_lib

# Normalize one raw JSON component dict to the canonical Component
# shape, filling the overlay defaults a typed Component schema would
# apply (replicas, leader_election, the empty image/registry/namespace
# the env overlays).
#
# Returns a plain dict — NOT a typed schema literal — because this runs
# inside a lambda body and KCL v0.11 rejects schema-literal
# instantiation there (see schema.k). The lib expanders take `any` and
# read fields dynamically, so a normalized dict is sufficient; projects
# that author components by hand still get the typed Server/Worker/…
# schemas at module scope.
_load_one = lambda raw: any -> any {
    {
        name = raw.name
        kind = raw.kind
        # Ports are a DEPLOY fact and components_gen.json carries none —
        # nothing forge discovers from the tree states a port. The empty
        # list is the honest base; the env main.k overlays real ports when
        # a component needs one, and Server's expansion falls back to the
        # standard :8080 mux when it does not.
        ports = []
        env = raw.env if "env" in raw else {}
        command = raw.command if "command" in raw else []
        schedule = raw.schedule if "schedule" in raw else ""
        group = raw.group if "group" in raw else ""
        version = raw.version if "version" in raw else ""
        crds = raw.crds if "crds" in raw else []
        # Polymorphic build block — passed through verbatim (a dict with a
        # `type` discriminator). Empty {} when the JSON omits it; the env
        # bridge then lets the Go side synthesize the ./cmd/<name> default.
        build = raw.build if "build" in raw else {}
        # Overlay defaults — the per-env main.k merges its env knobs on
        # top of these via the `|` operator.
        image = ""
        registry = ""
        namespace = ""
        replicas = 1
        env_vars = []
        platform = ""
        leader_election = True
    }
}

# Load + normalize the components emitted in components_gen.json. Each
# entry is a canonical Component dict ready for `render_components` (or
# a per-env `|` overlay).
load_components = lambda path: str -> [any] {
    _doc = json.decode(file.read(path))
    [_load_one(r) for r in _doc.components]
}

# Read just the project name from components_gen.json. Useful for env
# main.k files that need the project name for image/registry defaults.
load_project = lambda path: str -> str {
    json.decode(file.read(path)).project
}

# Read the project-level DEPLOY-TIME migration command from
# components_gen.json — the argv that applies this release's EMBEDDED
# schema migrations from inside the runtime image
# (`/app/<project> db migrate up`).
#
# forge emits it only when the project ships db/migrations/*.sql; an
# empty command is the honest "this project has no migrations" answer and
# renders NO migration step. A document written before this key existed
# reads as empty too, so an older components_gen.json is inert rather
# than a load error.
#
# Wire it into the env's ComponentEnv:
#
#     _component_env = fc.ComponentEnv {
#         namespace = "myapp-prod"
#         migrate = fc.load_migrate(fc.COMPONENTS_GEN)
#     }
load_migrate = lambda path: str -> [str] {
    _doc = json.decode(file.read(path))
    _m = _doc.migrate if "migrate" in _doc else {}
    _m.command if _m and "command" in _m else []
}

# The default location of the generated component data, relative to the
# project root (forge's kcl invocation cwd).
COMPONENTS_GEN = "deploy/kcl/components_gen.json"

schema ComponentEnv:
    """Per-environment knobs the component manifest renderer needs.
    Mirrors RenderEnv (schema.k) but keyed to the component model.

    namespace / registry are the env-wide deploy target; image_tag is
    the per-build override (forge passes `-D image_tag=<sha>`).

    `project` is the PROJECT name every rendered object is labelled
    with (`app.kubernetes.io/part-of`). It is the handle a CD pipeline
    uses to find one release's workloads — `kubectl get deploy -n <ns>
    -l app.kubernetes.io/part-of=<project>` — so it must be the
    project, not a component name. Defaults to "" (each object falls
    back to labelling itself), which is why the generated per-env
    main.k always sets it.
    """
    namespace: str
    project: str = ""
    registry: str = ""
    image_tag: str = "latest"
    network_policies: bool = True

    # `migrate` is the DEPLOY-TIME migration step: the argv that applies
    # this release's schema migrations, run as an initContainer on every
    # Deployment-shaped workload before its app container starts. Set it
    # from the generated data — `migrate = fc.load_migrate(fc.COMPONENTS_GEN)`
    # — so it follows the project (empty until the first .sql migration
    # exists, populated from then on) with no per-env edit.
    #
    # WHY an initContainer and not a Job: the ordering guarantee is
    # Kubernetes' own. It holds under `kubectl apply -f -`, under Argo CD,
    # under Flux, and under `forge env deploy` alike — no applier has to
    # cooperate, and there is no Job-name-per-release / Job-immutability
    # bookkeeping. Replicas racing the same migration are safe:
    # golang-migrate's postgres driver takes a session ADVISORY LOCK, so
    # one applies and the rest block, then find nothing to do. A migration
    # that FAILS wedges the init container, which stalls the rollout with
    # the old pods still serving — which is the correct outcome: shipping
    # code that needs a schema the database does not have is worse.
    #
    # Empty (the default) renders no init container at all — a migration
    # step running a command guaranteed to fail is worse than no step.
    migrate: [str] = []

    # --- NetworkPolicy inputs (read only when network_policies) ---
    #
    # `default-deny` blackholes every egress the allow-list misses, so
    # these are what keep an env's own database, collector and ingress
    # reachable. They are DECLARED rather than guessed: an empty
    # namespace omits its policy instead of rendering one against a
    # namespace that may not exist — a policy allowing traffic from a
    # namespace nothing runs in is a policy that denies it.
    #
    # egress_ports are TCP ports the workload may dial anywhere: 443 for
    # HTTPS (JWKS, object storage, third-party APIs) and 5432 for a
    # managed Postgres, which normally lives outside the cluster. Add
    # the port of any other external dependency (6379, 9092, …).
    egress_ports: [int] = [443, 5432]
    # Namespace running the OTLP collector the OTEL_EXPORTER_OTLP_ENDPOINT
    # points at. Empty when telemetry is not exported off-cluster.
    telemetry_namespace: str = ""
    # Namespace running the Gateway API / ingress controller that
    # terminates public traffic. Must match where the controller for the
    # env's `gateway_class_name` actually runs, or ingress is denied.
    ingress_namespace: str = ""

    check:
        namespace, "ComponentEnv.namespace is required"

_render_namespace = lambda env: ComponentEnv -> any {
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

# Normalize a list of typed components into the k8s manifest list for
# an environment. The Namespace comes first; each component expands per
# its kind; network policies (when on) close it out.
#
# `--target` group model: each component's own manifests carry their
# component (service) name (stamped in expand.k). The Namespace + network
# policies are env-shared and belong to no service, so they carry NO
# `app.kubernetes.io/name` — they apply on a bare deploy and are never
# selected by a service `--target`. No synthetic env/namespace group.
render_components = lambda components: [any], env: ComponentEnv -> [any] {
    [_render_namespace(env)] \
        + [item for c in components for item in comp_lib.render_component(c, env.namespace, env.registry, env.image_tag, env.project, env.migrate)] \
        + (netpol_lib.render_network_policies(
            env.namespace, env.egress_ports, env.telemetry_namespace, env.ingress_namespace
        ) if env.network_policies else [])
}
